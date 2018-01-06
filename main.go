package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kenshaw/evdev"
	"github.com/xo/terminfo"
	"golang.org/x/sync/errgroup"
)

const (
	sonyCorp   = 1356
	dualShock4 = 2508

	maxChecks = 15
)

func main() {
	flag.Parse()

	eg, ctxt := errgroup.WithContext(context.Background())
	eg.Go(func() error {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		select {
		case s := <-sigs:
			log.Printf("shutting down")
			return fmt.Errorf("received %v", s)
		case <-ctxt.Done():
		}
		return ctxt.Err()
	})
	eg.Go(run(ctxt))

	err := eg.Wait()
	if err != nil {
		log.Fatal(err)
	}
}

func run(ctxt context.Context) func() error {
	return func() error {
		ti, err := terminfo.LoadFromEnv()
		if err != nil {
			return fmt.Errorf("could not load terminal info: %v", err)
		}

		for i := 0; i < maxChecks; i++ {
			select {
			case <-ctxt.Done():
				return ctxt.Err()
			default:
			}

			btnDev, mtnDev, err := findDS4()
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			defer btnDev.Close()
			defer mtnDev.Close()

			/*log.Printf(">>> %s", btnDev.Name())
			log.Printf(">>> %s", btnDev.Path())
			log.Printf(">>> %s", btnDev.Serial())
			log.Printf(">>> %s", btnDev.Filepath())
			log.Printf(">>> bus: %s", btnDev.ID().BusType)

			log.Printf(">>> event types: %+v", btnDev.EventTypes())
			log.Printf(">>> sync types: %+v", btnDev.SyncTypes())
			log.Printf(">>> key types: %+v", btnDev.KeyTypes())
			log.Printf(">>> relative axis types: %+v", btnDev.RelativeTypes())
			log.Printf(">>> absolute axis types: %+v", btnDev.AbsoluteTypes())
			log.Printf(">>> effect types: %+v", btnDev.EffectTypes())
			log.Printf(">>> max effects: %d", btnDev.EffectMax())
			log.Printf(">>> leds: %+v", btnDev.LEDTypes())
			log.Printf(">>> powers: %+v", btnDev.PowerTypes())*/

			log.Printf("%s %s [%s]", btnDev.Serial(), btnDev.Name(), btnDev.Path())
			log.Printf("%s %s [%s]", mtnDev.Serial(), mtnDev.Name(), mtnDev.Path())

			err = runDS4(ctxt, ti, btnDev, mtnDev)
			if err != nil {
				log.Printf("stopped: %v", err)
			}
			i = 0
		}
		return fmt.Errorf("timeout waiting for gamepad")
	}
}

func findDS4() (*pad, *pad, error) {
	devices, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return nil, nil, err
	}

	var btnDev, mtnDev *evdev.Evdev
	var btnPath, mtnPath string
	for _, d := range devices {
		dev, err := evdev.OpenFile(d)
		if err != nil {
			continue
		}
		if id := dev.ID(); id.Vendor != sonyCorp && id.Product != dualShock4 {
			dev.Close()
			continue
		}
		if !strings.Contains(strings.ToLower(dev.Name()), "motion") {
			btnDev, btnPath = dev, d
		} else {
			mtnDev, mtnPath = dev, d
		}
	}

	if btnDev != nil && mtnDev != nil {
		return &pad{btnDev, btnPath}, &pad{mtnDev, mtnPath}, nil
	}
	if btnDev != nil {
		btnDev.Close()
	}
	if mtnDev != nil {
		mtnDev.Close()
	}
	return nil, nil, errors.New("no pad found")
}

type pad struct {
	*evdev.Evdev
	path string
}

func (p *pad) Filepath() string {
	return p.path
}

func runDS4(ctxt context.Context, ti *terminfo.Terminfo, btnDev, mtnDev *pad) error {
	defer func() {
		err := recover()
		termreset(ti)
		if err != nil {
			log.Fatal("unrecoverable error: %v", err)
		}
	}()

	btnAxes, mtnAxes := btnDev.AbsoluteTypes(), mtnDev.AbsoluteTypes()

	// set up terminal
	terminit(ti, fmt.Sprintf("DS4: %s - %s", btnDev.Name(), btnDev.Path()))
	termputs(ti, 1, 1, "Ctrl-C to exit")
	for _, v := range btns {
		termputs(ti, padTop+v.row, padLeft+colWidth*v.col, v.name)
	}

	// create context
	var cancel context.CancelFunc
	ctxt, cancel = context.WithCancel(ctxt)
	defer cancel()

	// start polling
	btnCh, err := btnDev.Poll(ctxt, 64)
	if err != nil {
		return err
	}
	mtnCh, err := mtnDev.Poll(ctxt, 64)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctxt.Done():
			return ctxt.Err()

		case event := <-btnCh:
			switch {
			// skip LZ + RZ
			case event.Type == evdev.EventKey && (event.Code == 0x138 || event.Code == 0x139):
			case event.Type == evdev.EventKey:
				v := btns[event.Code]
				n := v.name
				if event.Value == 1 {
					n = ti.Colorf(fg, bg, n)
				}
				termputs(ti, padTop+v.row, padLeft+colWidth*v.col, n)

			case event.Type == evdev.EventAbsolute:
				typ := evdev.AbsoluteType(event.Code)
				switch {
				case typ == evdev.AbsoluteHat0X || typ == evdev.AbsoluteHat0Y:
					for p, i := range []uint16{event.Code, event.Code + 2} {
						b := btns[i]
						n := b.name
						if (p == 0 && event.Value < 0) || (p == 1 && event.Value > 0) {
							n = ti.Colorf(fg, bg, n)
						}
						termputs(ti, padTop+b.row, padLeft+colWidth*b.col, n)
					}

				case typ == evdev.AbsoluteZ || typ == evdev.AbsoluteRZ:
					var v uint16 = 0x138
					if typ == evdev.AbsoluteRZ {
						v = 0x139
					}
					b := btns[v]
					n := b.name
					if event.Value > 0 {
						n = ti.Colorf(fg, int(event.Value), n)
					}
					termputs(ti, padTop+b.row, padLeft+colWidth*b.col, n)

				case typ == evdev.AbsoluteX || typ == evdev.AbsoluteY || typ == evdev.AbsoluteRX || typ == evdev.AbsoluteRY:
					a := btnAxes[typ]
					mid := a.Min + (a.Max-a.Min)/2
					var offset int
					if event.Value > mid {
						offset = 1
					}
					c := event.Code
					if c >= 3 {
						c++
					}
					for p, i := range []uint16{c, c + 2} {
						b := btns[i]
						n := b.name
						if v := abs(mid - event.Value); p == offset && v >= int(a.Flat) {
							n = ti.Colorf(fg, v%255, n)
						}
						termputs(ti, padTop+b.row, padLeft+colWidth*b.col, n)
					}
				}
			}

		case event := <-mtnCh:
			if event.Type != evdev.EventAbsolute {
				continue
			}
			typ := evdev.AbsoluteType(event.Code)
			v := fmt.Sprintf("% 4f", round(float64(event.Value)/float64(mtnAxes[typ].Res), 2))
			if len(v) > 6 {
				v = v[:6]
			}
			termputs(
				ti,
				mtnTop+int(event.Code)%3, mtnPadLeft+mtnColWidth*(int(event.Code)/3),
				"%s: %s", typ, v,
			)
		}
	}
}

// sl is the xterm+sl term.
var sl *terminfo.Terminfo

// termtitle sets the title on the window.
func termtitle(w io.Writer, ti *terminfo.Terminfo, n string) {
	var once sync.Once
	once.Do(func() {
		if strings.Contains(strings.ToLower(os.Getenv("TERM")), "xterm") || os.Getenv("COLORTERM") == "truecolor" {
			sl, _ = terminfo.Load("xterm+sl")
		}
	})
	if sl != nil {
		ti = sl
	}
	if ti.Has(terminfo.HasStatusLine) {
		ti.Fprintf(w, terminfo.ToStatusLine)
		fmt.Fprint(w, n)
		ti.Fprintf(w, terminfo.FromStatusLine)
	}
}

// terminit initializes a terminal.
func terminit(ti *terminfo.Terminfo, n string) {
	buf := new(bytes.Buffer)
	ti.Fprintf(buf, terminfo.EnterCaMode)
	ti.Fprintf(buf, terminfo.ClearScreen)
	ti.Fprintf(buf, terminfo.CursorHome)
	ti.Fprintf(buf, terminfo.CursorInvisible)
	termtitle(buf, ti, n)
	os.Stdout.Write(buf.Bytes())
}

// termreset resets the terminal.
func termreset(ti *terminfo.Terminfo) {
	buf := new(bytes.Buffer)
	ti.Fprintf(buf, terminfo.CursorNormal)
	ti.Fprintf(buf, terminfo.ExitCaMode)
	os.Stdout.Write(buf.Bytes())
}

// termputs writes a field at row, col.
func termputs(ti *terminfo.Terminfo, row, col int, s string, v ...interface{}) {
	buf := new(bytes.Buffer)
	ti.Fprintf(buf, terminfo.CursorAddress, row, col)
	fmt.Fprintf(buf, s, v...)
	os.Stdout.Write(buf.Bytes())
}

// round rounds f to the nearest digits.
func round(f float64, n int) float64 {
	z := math.Pow10(n)
	return math.Round(f*z) / z
}

func abs(z int32) int {
	return int(math.Abs(float64(z)))
}

// btns contains the button names and layout positions.
var btns = map[uint16]struct {
	name     string
	row, col int
}{
	0x130: {" ⨉ ", 5, 11},
	0x131: {"○  ", 4, 12},
	0x133: {" △ ", 3, 11},
	0x134: {"  □", 4, 10},

	0x136: {" L¹", 1, 1},
	0x137: {" R¹", 1, 11},
	0x138: {" Lᶻ", 0, 1},
	0x139: {" Rᶻ", 0, 11},

	0x13a: {"SHARE", 2, 3},
	0x13b: {"\b OPTS", 2, 9},
	0x13c: {" ㎰ ", 8, 6},

	0x13d: {" L³", 8, 3},
	0x13e: {" R³", 8, 9},

	// dpad
	0x10: {"  ←", 4, 0},
	0x11: {" ↑ ", 3, 1},
	0x12: {"→ ", 4, 2},
	0x13: {" ↓ ", 5, 1},

	// sticks
	0: {"  ←", 8, 2},
	1: {" ↑ ", 7, 3},
	2: {"→  ", 8, 4},
	3: {" ↓ ", 9, 3},
	4: {"  ←", 8, 8},
	5: {" ↑ ", 7, 9},
	6: {"→  ", 8, 10},
	7: {" ↓ ", 9, 9},
}

const (
	padTop      = 4
	padLeft     = 4
	colWidth    = 4
	mtnTop      = 16
	mtnPadLeft  = 3*colWidth + 2
	mtnColWidth = 6*colWidth - 2
	fg          = 0xff
	bg          = 0xef
)
