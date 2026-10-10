package ui

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"

	"modbus-ai-studio/internal/modbus"
	"modbus-ai-studio/internal/simulator"
	"modbus-ai-studio/platform"
)

type connectionTestTheme struct {
	fyne.Theme
	variant fyne.ThemeVariant
}

func (th connectionTestTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	return th.Theme.Color(name, th.variant)
}

// Connecting locks the configuration, but the endpoint and protocol still need
// to be readable. Check actual rendered pixels, including the serial controls.
func TestConnectionParametersRemainReadableWhenLocked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		variant fyne.ThemeVariant
	}{{"light", theme.VariantLight}, {"dark", theme.VariantDark}} {
		t.Run(tc.name, func(t *testing.T) {
			a := test.NewTempApp(t)
			a.Settings().SetTheme(theme.DefaultTheme())
			platform.ConfigureApp(a)
			a.Settings().SetTheme(connectionTestTheme{a.Settings().Theme(), tc.variant})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := simulator.NewServer(modbus.ModeTCP, 1, simulator.NewStore(16))
			go srv.Serve(ln)
			t.Cleanup(func() { _ = ln.Close(); _ = srv.Close() })
			ws := openWS(t, a, false)
			target := ln.Addr().String()
			locked(func() {
				ws.useSim.SetChecked(false)
				ws.proto.SetSelected(protoTCP)
				ws.target.SetText(target)
				ws.timeoutE.SetText("1500")
				ws.win.Resize(fyne.NewSize(960, 620))
				captureConnectionBar(t, ws, "connection-"+tc.name+"-before")
				ws.connect()
			})
			waitFor(t, 5*time.Second, "connected", func() bool { return ws.session != nil })
			locked(func() {
				if !ws.proto.Disabled() || !ws.target.Disabled() || ws.timeoutE.Disabled() {
					t.Error("connection must lock protocol and endpoint while keeping timeout editable")
				}
				if ws.proto.Selected != protoTCP || ws.target.Text != target || ws.timeoutE.Text != "1500" {
					t.Error("connection changed the displayed configuration")
				}
				img := captureConnectionBar(t, ws, "connection-"+tc.name+"-connected")
				assertReadableConnectionText(t, img, ws.proto, "protocol")
				assertReadableConnectionText(t, img, ws.target, "endpoint")
				assertReadableConnectionText(t, img, ws.timeoutE, "timeout")
				ws.disconnect()
				if ws.proto.Disabled() || ws.target.Disabled() {
					t.Error("disconnect must restore editable connection parameters")
				}
				ws.proto.SetSelected(protoRTU)
				ws.port.SetOptions([]string{"COM7"})
				ws.port.SetSelected("COM7")
				ws.baud.SetText("19200")
				ws.frameFmt.SetSelected("8E1")
				ws.setInputsEnabled(false)
				if ws.port.Selected != "COM7" || ws.baud.Text != "19200" || ws.frameFmt.Selected != "8E1" {
					t.Error("locking changed the displayed serial configuration")
				}
				img = captureConnectionBar(t, ws, "connection-"+tc.name+"-serial-locked")
				for _, field := range []struct {
					object fyne.CanvasObject
					name   string
				}{{ws.proto, "serial protocol"}, {ws.port, "port"}, {ws.baud, "baud"}, {ws.frameFmt, "frame format"}} {
					if !field.object.(fyne.Disableable).Disabled() {
						t.Errorf("%s must stay locked while displaying the configuration", field.name)
					}
					assertReadableConnectionText(t, img, field.object, field.name)
				}
			})
		})
	}
}

func captureConnectionBar(t *testing.T, ws *Workspace, name string) image.Image {
	t.Helper()
	img := ws.win.Canvas().Capture()
	if dir := os.Getenv("MODBUS_AI_SNAPSHOT"); dir != "" {
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return img
}

func assertReadableConnectionText(t *testing.T, img image.Image, control fyne.CanvasObject, name string) {
	t.Helper()
	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(control)
	size := control.Size()
	// Exclude borders and the dropdown arrow, leaving only the text interior.
	bounds := image.Rect(int(pos.X)+8, int(pos.Y)+6, int(pos.X+size.Width)-32, int(pos.Y+size.Height)-6)
	if bounds.Empty() || !bounds.In(img.Bounds()) {
		t.Errorf("%s is outside the visible connection bar: %v", name, bounds)
		return
	}
	counts := make(map[color.NRGBA]int)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			counts[color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)]++
		}
	}
	var background color.NRGBA
	for c, count := range counts {
		if count > counts[background] {
			background = c
		}
	}
	readablePixels := 0
	for c, count := range counts {
		if connectionTextContrast(c, background) >= 4.5 {
			readablePixels += count
		}
	}
	if readablePixels < 20 {
		t.Errorf("%s becomes unreadable when locked: only %d text pixels reach 4.5:1 contrast", name, readablePixels)
	}
}

func connectionTextContrast(a, b color.NRGBA) float64 {
	luminance := func(c color.NRGBA) float64 {
		linear := func(v uint8) float64 {
			x := float64(v) / 255
			if x <= .04045 {
				return x / 12.92
			}
			return math.Pow((x+.055)/1.055, 2.4)
		}
		return .2126*linear(c.R) + .7152*linear(c.G) + .0722*linear(c.B)
	}
	x, y := luminance(a), luminance(b)
	return (max(x, y) + .05) / (min(x, y) + .05)
}
