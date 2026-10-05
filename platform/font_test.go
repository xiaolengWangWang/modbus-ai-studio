package platform

import (
	"bytes"
	"os"
	"runtime"
	"testing"

	"fyne.io/fyne/v2/theme"
	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
)

func TestStandaloneFont(t *testing.T) {
	inputs := [][]byte{theme.DefaultTextFont().Content()}
	if runtime.GOOS == "darwin" {
		if ttc, err := os.ReadFile("/System/Library/Fonts/Avenir Next.ttc"); err == nil {
			inputs = append(inputs, ttc)
		}
	}
	for _, input := range inputs {
		loaders, err := ot.NewLoaders(bytes.NewReader(input))
		if err != nil || len(loaders) == 0 {
			t.Fatalf("font loaders: %v", err)
		}
		out, err := standaloneFont(loaders[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := font.ParseTTF(bytes.NewReader(out)); err != nil {
			t.Fatalf("Fyne font parser rejected standalone font: %v", err)
		}
		if sum := fontChecksum(out); sum != 0xB1B0AFBA {
			t.Fatalf("font checksum: %08X", sum)
		}
	}
}
