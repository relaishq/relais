// Command generate synthesizes the demo's owned sine-wave assets. Run from the
// repository root: go run ./pkg/agent/demo/generate. Requires ffmpeg with libopus.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func generate() error {
	for tone := 0; tone < 11; tone++ {
		frames := 25
		if tone == 10 {
			frames = 5
		}
		pcm := make([]byte, frames*960*2)
		for i := 0; i < len(pcm)/2; i++ {
			t := float64(i) / 48000
			freq := 330 * math.Pow(2, float64(tone)/12)
			if tone == 0 { // Every tenth count: two short high/low pulses.
				freq = 660
				if (i/2400)%2 == 1 {
					freq = 330
				}
			}
			if tone == 10 {
				freq = 1100 + 2200*t
			} // Resume chime.
			envelope := math.Min(1, math.Min(float64(i)/240, float64(len(pcm)/2-1-i)/240))
			value := int16(7000 * envelope * math.Sin(2*math.Pi*freq*t))
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(value))
		}
		cmd := exec.Command("ffmpeg", "-v", "error", "-f", "s16le", "-ar", "48000", "-ac", "1", "-i", "pipe:0", "-c:a", "libopus", "-b:a", "32k", "-cutoff", "4000", "-vbr", "off", "-frame_duration", "20", "-application", "lowdelay", "-page_duration", "20000", "-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact", "-f", "ogg", "pipe:1")
		cmd.Stdin = bytes.NewReader(pcm)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("ffmpeg: %w: %s", err, stderr.String())
		}
		reader, _, err := oggreader.NewWith(bytes.NewReader(data))
		if err != nil {
			return err
		}
		var packets bytes.Buffer
		n := 0
		for n < frames {
			packet, header, err := reader.ParseNextPage()
			if err != nil {
				return err
			}
			if _, isHeader := header.HeaderType(packet); isHeader {
				continue
			}
			if len(packet) == 0 || packet[0] != 0x98 {
				return fmt.Errorf("expected a mono narrowband CELT 20 ms frame (TOC 0x98)")
			}
			if err := binary.Write(&packets, binary.BigEndian, uint16(len(packet))); err != nil {
				return err
			}
			if _, err := packets.Write(packet); err != nil {
				return err
			}
			n++
		}
		if err := os.WriteFile(fmt.Sprintf("pkg/agent/demo/assets/%02d.opuspackets", tone), packets.Bytes(), 0644); err != nil {
			return err
		}
	}
	return nil
}
