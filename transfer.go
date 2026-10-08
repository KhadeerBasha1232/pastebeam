package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Message types inside the encrypted channel.
const (
	msgOffer   = 'O' // sender -> receiver: JSON offer
	msgAccept  = 'A' // receiver -> sender
	msgReject  = 'R' // receiver -> sender
	msgData    = 'D' // sender -> receiver: a chunk of the stream
	msgEnd     = 'E' // sender -> receiver: SHA-256 of everything sent
	msgOK      = 'K' // receiver -> sender: saved and verified
	msgFailure = 'X' // receiver -> sender: error text
)

const (
	chunkSize      = 64 << 10
	maxBadAttempts = 3
)

var errDeclined = errors.New("the receiver declined the transfer")

// connected stops background discovery from printing once a peer is found.
var connected atomic.Bool

type offer struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`  // file size, or total size of the files in a dir
	Dir   bool   `json:"dir"`   // if true the stream is a tar archive
	Files int    `json:"files"` // number of files in a dir
}

// ---------------------------------------------------------------- send

func runSend(ctx context.Context, path, code string) error {
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	of := offer{Name: filepath.Base(path), Dir: st.IsDir()}
	if of.Name == "." || of.Name == string(filepath.Separator) {
		abs, _ := filepath.Abs(path)
		of.Name = filepath.Base(abs)
	}
	if of.Dir {
		of.Size, of.Files, err = dirSize(path)
		if err != nil {
			return err
		}
	} else if st.Mode().IsRegular() {
		of.Size = st.Size()
	} else {
		return fmt.Errorf("%s is not a regular file or directory", path)
	}

	if code == "" {
		if code, err = generateCode(); err != nil {
			return err
		}
	}
	code, nameplate, err := parseCode(code)
	if err != nil {
		return err
	}

	header("send files with a short code")
	out.Println("  " + sBold.Render(of.Name) + "  " + sDim.Render(describeOffer(of)))
	blank()
	out.Println(box(sDim.Render("your code"), sCyan.Render(code)))
	blank()
	out.Println("  On the other machine run:")
	out.Println("    " + sAccent.Render("pastebeam get "+code))
	blank()

	n, err := newNode(ctx, true)
	if err != nil {
		return err
	}
	defer n.Close()

	result := make(chan error, 1)
	var busy atomic.Bool
	var bad atomic.Int32
	n.host.SetStreamHandler(protocolID, func(s network.Stream) {
		if !busy.CompareAndSwap(false, true) {
			s.Reset()
			return
		}
		defer busy.Store(false)
		err := serveTransfer(s, code, path, of)
		if errors.Is(err, errBadCode) {
			s.Reset()
			left := maxBadAttempts - bad.Add(1)
			if left <= 0 {
				result <- errors.New("too many wrong codes, giving up (someone may be guessing your code)")
				return
			}
			warn("Someone tried a wrong code %s", sDim.Render(fmt.Sprintf("(%d attempts left)", left)))
			out.Spin("Waiting for the receiver…")
			return
		}
		if err != nil {
			s.Reset()
		}
		result <- err
	})

	if stop, err := n.startMDNS(nameplate, func(peer.AddrInfo) {}); err != nil {
		warn("LAN discovery unavailable: %v", err)
	} else {
		defer stop()
		step("Visible on your local network")
	}
	out.Spin("Waiting for the receiver…")
	go advertise(ctx, n, nameplate)

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// advertise publishes the nameplate on the public DHT and keeps it fresh.
func advertise(ctx context.Context, n *node, nameplate string) {
	n.joinDHT(ctx)
	if ctx.Err() != nil {
		return
	}
	if !connected.Load() {
		step("Joined the peer-to-peer network %s", sDim.Render(fmt.Sprintf("· %d peers", n.dht.RoutingTable().Size())))
	}
	if n.waitForRelay(ctx, 45*time.Second) {
		if !connected.Load() {
			step("Relay reserved for NAT traversal")
		}
	} else if ctx.Err() == nil && !connected.Load() {
		warn("No public relay yet %s", sDim.Render("· receivers behind strict NAT may not reach us"))
	}
	announced := false
	for ctx.Err() == nil {
		_, err := n.disc.Advertise(ctx, dhtNamespace(nameplate))
		wait := 5 * time.Minute
		if err != nil {
			wait = 10 * time.Second
		} else if !announced {
			announced = true
			if !connected.Load() {
				step("Reachable over the internet")
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

func serveTransfer(s network.Stream, code, path string, of offer) error {
	s.SetDeadline(time.Now().Add(time.Minute))
	sc, err := handshake(s, code, true, []byte(s.Conn().LocalPeer()), []byte(s.Conn().RemotePeer()))
	if err != nil {
		return err
	}
	s.SetDeadline(time.Time{})
	connected.Store(true)
	step("Receiver connected %s", sDim.Render("· "+describeConn(s.Conn())))
	step("Code verified %s", sDim.Render("· SPAKE2 + ChaCha20-Poly1305, end-to-end encrypted"))
	out.Spin("Waiting for the receiver to accept…")

	ob, _ := json.Marshal(of)
	if err := sc.WriteMsg(msgOffer, ob); err != nil {
		return err
	}
	typ, _, err := sc.ReadMsg()
	if err != nil {
		return err
	}
	if typ == msgReject {
		return errDeclined
	}
	if typ != msgAccept {
		return fmt.Errorf("unexpected message %q", typ)
	}

	var src io.ReadCloser
	if of.Dir {
		src = tarDir(path)
	} else if src, err = os.Open(path); err != nil {
		return err
	}
	defer src.Close()

	h := sha256.New()
	prog := newProgress("Sent", of.Size)
	buf := make([]byte, chunkSize)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			if err := sc.WriteMsg(msgData, buf[:n]); err != nil {
				out.StopLive()
				return err
			}
			prog.Add(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.StopLive()
			return rerr
		}
	}
	prog.Finish()
	if err := sc.WriteMsg(msgEnd, h.Sum(nil)); err != nil {
		return err
	}

	out.Spin("Waiting for the receiver to verify…")
	typ, payload, err := sc.ReadMsg()
	out.StopLive()
	if err != nil {
		return err
	}
	if typ == msgFailure {
		return fmt.Errorf("receiver failed: %s", payload)
	}
	if typ != msgOK {
		return fmt.Errorf("unexpected message %q", typ)
	}
	s.Close()
	step("Receiver verified the SHA-256 checksum")
	out.Println("\n  " + sOK.Render("Done!") + " " + sDim.Render(of.Name+" was delivered.") + "\n")
	return nil
}

// ---------------------------------------------------------------- receive

// errTryLater: the candidate couldn't be reached or isn't a sender for us.
var errTryLater = errors.New("peer not usable")

func runGet(ctx context.Context, code, outDir string, yes bool, timeout time.Duration) error {
	code, nameplate, err := parseCode(code)
	if err != nil {
		return err
	}
	if st, err := os.Stat(outDir); err != nil || !st.IsDir() {
		return fmt.Errorf("output directory %q does not exist", outDir)
	}

	header("receive files with a short code")
	n, err := newNode(ctx, false)
	if err != nil {
		return err
	}
	defer n.Close()

	candidates := make(chan peer.AddrInfo, 64)
	push := func(ai peer.AddrInfo) {
		if ai.ID == n.host.ID() {
			return
		}
		select {
		case candidates <- ai:
		default:
		}
	}
	if stop, err := n.startMDNS(nameplate, push); err != nil {
		warn("LAN discovery unavailable: %v", err)
	} else {
		defer stop()
		step("Searching your local network")
	}
	go func() {
		n.joinDHT(ctx)
		if ctx.Err() == nil && !connected.Load() {
			step("Searching the internet %s", sDim.Render(fmt.Sprintf("· joined the p2p network, %d peers", n.dht.RoutingTable().Size())))
		}
		for ctx.Err() == nil {
			if ch, err := n.disc.FindPeers(ctx, dhtNamespace(nameplate)); err == nil {
				for ai := range ch {
					push(ai)
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}()

	out.Spin("Looking for the sender…")
	deadline := time.After(timeout)
	lastTry := map[peer.ID]time.Time{}
	rejected := map[peer.ID]bool{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			if len(rejected) > 0 {
				return errors.New("the sender is there but the code didn't match; double-check it")
			}
			return errors.New("couldn't reach the sender; check the code and that `pastebeam send` is still running")
		case ai := <-candidates:
			if rejected[ai.ID] || time.Since(lastTry[ai.ID]) < 10*time.Second {
				continue
			}
			lastTry[ai.ID] = time.Now()
			err := tryReceive(ctx, n, ai, code, outDir, yes)
			switch {
			case errors.Is(err, errBadCode):
				rejected[ai.ID] = true
				warn("Found a sender with a different code %s", sDim.Render("· typo? still looking"))
				out.Spin("Looking for the sender…")
			case errors.Is(err, errTryLater):
				out.Spin("Looking for the sender…")
			default:
				return err
			}
		}
	}
}

func tryReceive(ctx context.Context, n *node, ai peer.AddrInfo, code, outDir string, yes bool) error {
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cctx = network.WithDialPeerTimeout(cctx, time.Minute)

	out.Spin("Connecting to a possible sender…")
	if len(ai.Addrs) == 0 {
		found, err := n.dht.FindPeer(cctx, ai.ID)
		if err != nil {
			return errTryLater
		}
		ai = found
	}
	if err := n.host.Connect(cctx, ai); err != nil {
		return errTryLater
	}
	// If we only reached the sender through a relay, NewStream waits here
	// while libp2p hole-punches a direct connection.
	out.Spin("Opening a direct connection…")
	s, err := n.host.NewStream(cctx, ai.ID, protocolID)
	if err != nil {
		if errors.Is(err, network.ErrLimitedConn) {
			warn("Reached the sender only through a relay; hole punching failed %s", sDim.Render("· retrying"))
		}
		return errTryLater
	}
	done := false
	defer func() {
		if !done {
			s.Reset()
		}
	}()

	out.Spin("Verifying the code…")
	s.SetDeadline(time.Now().Add(time.Minute))
	sc, err := handshake(s, code, false, []byte(ai.ID), []byte(n.host.ID()))
	if errors.Is(err, errBadCode) {
		return err
	}
	if err != nil {
		return errTryLater
	}
	s.SetDeadline(time.Time{})
	connected.Store(true)
	step("Connected to the sender %s", sDim.Render("· "+describeConn(s.Conn())))
	step("Code verified %s", sDim.Render("· SPAKE2 + ChaCha20-Poly1305, end-to-end encrypted"))

	typ, payload, err := sc.ReadMsg()
	if err != nil {
		return err
	}
	var of offer
	if typ != msgOffer || json.Unmarshal(payload, &of) != nil {
		return errors.New("bad offer from sender")
	}
	if !safeName(of.Name) {
		return fmt.Errorf("sender offered an unsafe file name %q", of.Name)
	}

	out.StopLive()
	blank()
	out.Println(box(sBold.Render(of.Name), sDim.Render(describeOffer(of))))
	blank()
	if !yes && !confirm("Accept this "+map[bool]string{true: "folder", false: "file"}[of.Dir]+"?") {
		sc.WriteMsg(msgReject, nil)
		done = true
		hangUp(s)
		return errors.New("transfer declined")
	}
	if err := sc.WriteMsg(msgAccept, nil); err != nil {
		return err
	}

	dest := uniquePath(filepath.Join(outDir, of.Name))
	dr := &dataReader{c: sc, h: sha256.New(), prog: newProgress("Received", of.Size)}
	if of.Dir {
		err = receiveDir(dr, outDir, dest)
	} else {
		err = receiveFile(dr, outDir, dest)
	}
	out.StopLive()
	if err != nil {
		sc.WriteMsg(msgFailure, []byte(err.Error()))
		return err
	}
	if err := sc.WriteMsg(msgOK, nil); err != nil {
		return err
	}
	done = true
	hangUp(s)
	step("SHA-256 checksum verified")
	out.Println("\n  " + sOK.Render("Saved") + " " + sBold.Render(dest) + "\n")
	return nil
}

// hangUp waits for the sender to close first, so our last message isn't lost
// when this process exits and tears down the connection.
func hangUp(s network.Stream) {
	s.CloseWrite()
	s.SetReadDeadline(time.Now().Add(10 * time.Second))
	io.Copy(io.Discard, s)
	s.Close()
}

func receiveFile(dr *dataReader, outDir, dest string) error {
	tmp, err := os.CreateTemp(outDir, ".pastebeam-*.part")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, dr)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = dr.verify()
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dest)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func receiveDir(dr *dataReader, outDir, dest string) error {
	tmp, err := os.MkdirTemp(outDir, ".pastebeam-*.part")
	if err != nil {
		return err
	}
	err = untar(dr, tmp)
	if err == nil {
		_, err = io.Copy(io.Discard, dr) // tar padding, up to the end marker
	}
	if err == nil {
		err = dr.verify()
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		os.RemoveAll(tmp)
	}
	return err
}

// dataReader turns the stream of encrypted data messages back into bytes.
type dataReader struct {
	c     *secureConn
	h     hash.Hash
	prog  *progress
	buf   []byte
	done  bool
	final []byte
}

func (r *dataReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		typ, payload, err := r.c.ReadMsg()
		if err != nil {
			return 0, err
		}
		switch typ {
		case msgData:
			r.buf = payload
			r.h.Write(payload)
			r.prog.Add(len(payload))
		case msgEnd:
			r.done = true
			r.final = payload
			r.prog.Finish()
		default:
			return 0, fmt.Errorf("unexpected message %q", typ)
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *dataReader) verify() error {
	if !r.done || !bytes.Equal(r.h.Sum(nil), r.final) {
		return errors.New("checksum mismatch, transfer corrupted")
	}
	return nil
}

// ---------------------------------------------------------------- folders

func dirSize(root string) (size int64, files int, err error) {
	err = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		files++
		return nil
	})
	return
}

// tarDir streams root as a tar archive. Symlinks and special files are skipped.
func tarDir(root string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == "." {
				return err
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
			if d.IsDir() {
				hdr.Typeflag, hdr.Name = tar.TypeDir, hdr.Name+"/"
				return tw.WriteHeader(hdr)
			}
			hdr.Typeflag, hdr.Size = tar.TypeReg, info.Size()
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.CopyN(tw, f, info.Size())
			return err
		})
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	return pr
}

// untar extracts into root, refusing anything that would escape it.
func untar(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(root, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}

// ---------------------------------------------------------------- helpers

func safeName(name string) bool {
	return name != "" && !strings.ContainsAny(name, `/\:`) && filepath.IsLocal(name)
}

// uniquePath returns p, or "name (1).ext", "name (2).ext", ... if p exists.
func uniquePath(p string) string {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(c); errors.Is(err, fs.ErrNotExist) {
			return c
		}
	}
}
