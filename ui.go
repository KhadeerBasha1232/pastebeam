package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/muesli/termenv"
	ma "github.com/multiformats/go-multiaddr"
	"golang.org/x/term"
)

// All UI goes to stderr. On a terminal there is one "live" line at the
// bottom (spinner or progress bar) that permanent lines scroll above.

var (
	re = lipgloss.NewRenderer(os.Stderr)

	colAccent = lipgloss.Color("#8B5CF6")
	colCyan   = lipgloss.Color("#22D3EE")
	colGreen  = lipgloss.Color("#34D399")
	colYellow = lipgloss.Color("#FBBF24")
	colRed    = lipgloss.Color("#F87171")
	colDim    = lipgloss.Color("#7C8594")

	sLogo   = re.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(colAccent).Padding(0, 1)
	sDim    = re.NewStyle().Foreground(colDim)
	sBold   = re.NewStyle().Bold(true)
	sAccent = re.NewStyle().Foreground(colAccent).Bold(true)
	sCyan   = re.NewStyle().Foreground(colCyan).Bold(true)
	sOK     = re.NewStyle().Foreground(colGreen).Bold(true)
	sWarn   = re.NewStyle().Foreground(colYellow).Bold(true)
	sErr    = re.NewStyle().Foreground(colRed).Bold(true)
	sBox    = re.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colAccent).Padding(0, 3).MarginLeft(2)
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type ui struct {
	mu    sync.Mutex
	tty   bool
	live  func() string
	drawn bool
	frame int
}

var out = newUI()

func newUI() *ui {
	u := &ui{tty: term.IsTerminal(int(os.Stderr.Fd()))}
	// CLICOLOR_FORCE=1 keeps full colors when output is piped or recorded.
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		re.SetColorProfile(termenv.TrueColor)
	}
	if u.tty {
		termenv.EnableVirtualTerminalProcessing(termenv.NewOutput(os.Stderr))
		go func() {
			for range time.Tick(100 * time.Millisecond) {
				u.mu.Lock()
				u.frame++
				u.redraw()
				u.mu.Unlock()
			}
		}()
	}
	return u
}

func (u *ui) redraw() {
	if !u.tty || u.live == nil {
		return
	}
	fmt.Fprint(os.Stderr, "\r\x1b[2K"+u.live())
	u.drawn = true
}

func (u *ui) clear() {
	if u.drawn {
		fmt.Fprint(os.Stderr, "\r\x1b[2K")
		u.drawn = false
	}
}

// Println prints a permanent line above the live line.
func (u *ui) Println(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clear()
	fmt.Fprintln(os.Stderr, s)
	u.redraw()
}

// Spin shows a spinner with text on the live line.
func (u *ui) Spin(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.tty {
		fmt.Fprintln(os.Stderr, "  ... "+text)
		return
	}
	u.live = func() string {
		return "  " + sAccent.Render(spinnerFrames[u.frame%len(spinnerFrames)]) + " " + sDim.Render(text)
	}
	u.redraw()
}

func (u *ui) setLive(f func() string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.live = f
	u.redraw()
}

func (u *ui) StopLive() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.clear()
	u.live = nil
}

func step(format string, args ...any) {
	out.Println("  " + sOK.Render("✓") + " " + fmt.Sprintf(format, args...))
}

func warn(format string, args ...any) {
	out.Println("  " + sWarn.Render("!") + " " + fmt.Sprintf(format, args...))
}

func failLine(err error) {
	out.StopLive()
	out.Println("\n  " + sErr.Render("✗ "+err.Error()))
}

func header(subtitle string) {
	out.Println("\n  " + sLogo.Render("▲ pastebeam") + "  " + sDim.Render(subtitle) + "\n")
}

func blank() { out.Println("") }

func box(lines ...string) string {
	return sBox.Render(strings.Join(lines, "\n"))
}

func describeOffer(of offer) string {
	if of.Dir {
		return fmt.Sprintf("folder · %d files · %s", of.Files, humanBytes(of.Size))
	}
	return "file · " + humanBytes(of.Size)
}

// describeConn turns a libp2p connection into "direct · QUIC · LAN 192.168.1.5".
func describeConn(c network.Conn) string {
	addr := c.RemoteMultiaddr()
	s := addr.String()

	kind := "direct"
	if c.Stat().Limited || strings.Contains(s, "/p2p-circuit") {
		kind = "relayed"
	}
	transport := "TCP"
	switch {
	case strings.Contains(s, "/webrtc"):
		transport = "WebRTC"
	case strings.Contains(s, "/webtransport"):
		transport = "WebTransport"
	case strings.Contains(s, "/quic"):
		transport = "QUIC"
	}
	ip, err := addr.ValueForProtocol(ma.P_IP4)
	if err != nil {
		ip, _ = addr.ValueForProtocol(ma.P_IP6)
	}
	where := "internet"
	if p := net.ParseIP(ip); p != nil && (p.IsPrivate() || p.IsLoopback() || p.IsLinkLocalUnicast()) {
		where = "LAN"
	}
	return fmt.Sprintf("%s · %s · %s %s", kind, transport, where, ip)
}

func confirm(question string) bool {
	out.StopLive()
	fmt.Fprint(os.Stderr, "  "+sCyan.Render("?")+" "+sBold.Render(question)+" "+sDim.Render("[Y/n] "))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// ---------------------------------------------------------------- progress

type progress struct {
	verb  string // "Sent" or "Received"
	total int64
	done  atomic.Int64
	start time.Time
}

func newProgress(verb string, total int64) *progress {
	p := &progress{verb: verb, total: total, start: time.Now()}
	out.setLive(p.render)
	return p
}

func (p *progress) Add(n int) { p.done.Add(int64(n)) }

func (p *progress) Finish() {
	out.StopLive()
	el := time.Since(p.start)
	done := p.done.Load()
	step("%s %s in %s %s", p.verb, sBold.Render(humanBytes(done)), el.Round(100*time.Millisecond),
		sDim.Render("· "+humanBytes(int64(float64(done)/max(el.Seconds(), 0.001)))+"/s"))
}

func (p *progress) render() string {
	done := p.done.Load()
	rate := float64(done) / max(time.Since(p.start).Seconds(), 0.001)
	frac := 1.0
	if p.total > 0 {
		frac = min(1, float64(done)/float64(p.total))
	}
	info := fmt.Sprintf("%s / %s · %s/s", humanBytes(done), humanBytes(p.total), humanBytes(int64(rate)))
	if p.total > done && rate > 0 {
		eta := time.Duration(float64(p.total-done) / rate * float64(time.Second))
		info += " · " + eta.Round(time.Second).String() + " left"
	}
	return "  " + gradientBar(frac, 32) + " " + sBold.Render(fmt.Sprintf("%3.0f%%", frac*100)) + "  " + sDim.Render(info)
}

// gradientBar draws a bar that fades from violet to cyan.
func gradientBar(frac float64, width int) string {
	filled := int(frac * float64(width))
	var b strings.Builder
	for i := range width {
		if i < filled {
			t := float64(i) / float64(max(width-1, 1))
			b.WriteString(re.NewStyle().Foreground(lerpColor(0x8B5CF6, 0x22D3EE, t)).Render("━"))
		} else {
			b.WriteString(sDim.Render("─"))
		}
	}
	return b.String()
}

func lerpColor(a, b uint32, t float64) lipgloss.Color {
	ch := func(shift uint) uint32 {
		x, y := float64((a>>shift)&0xff), float64((b>>shift)&0xff)
		return uint32(x + (y-x)*t)
	}
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", ch(16), ch(8), ch(0)))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
