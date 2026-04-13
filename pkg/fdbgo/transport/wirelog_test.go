package transport

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/onsi/gomega"
)

func TestWireLogBinary(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	var buf bytes.Buffer
	wl := &WireLog{w: &buf, binary: true}

	token := UID{First: 0x1234, Second: 0x5678}
	body := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	wl.log('S', token, body)

	data := buf.Bytes()
	g.Expect(len(data)).To(gomega.BeNumerically(">=", 29+4)) // header + body

	// Direction byte
	g.Expect(data[0]).To(gomega.Equal(byte('S')))

	// Token
	first := binary.LittleEndian.Uint64(data[9:17])
	second := binary.LittleEndian.Uint64(data[17:25])
	g.Expect(first).To(gomega.Equal(uint64(0x1234)))
	g.Expect(second).To(gomega.Equal(uint64(0x5678)))

	// Body length
	bodyLen := binary.LittleEndian.Uint32(data[25:29])
	g.Expect(bodyLen).To(gomega.Equal(uint32(4)))

	// Body content
	g.Expect(data[29:33]).To(gomega.Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
}

func TestWireLogText(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	var buf bytes.Buffer
	wl := &WireLog{w: &buf, binary: false}

	token := UID{First: 0xABCD, Second: 0xEF01}
	body := []byte{0x01, 0x02, 0x03}
	wl.log('S', token, body)

	output := buf.String()
	g.Expect(output).To(gomega.ContainSubstring("SEND"))
	g.Expect(output).To(gomega.ContainSubstring("len=3"))
}

func TestWireLogTextRecv(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	var buf bytes.Buffer
	wl := &WireLog{w: &buf, binary: false}

	token := UID{First: 1, Second: 2}
	body := []byte{0xFF}
	wl.log('R', token, body)

	output := buf.String()
	g.Expect(output).To(gomega.ContainSubstring("RECV"))
}

func TestWireLogTextTruncation(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	var buf bytes.Buffer
	wl := &WireLog{w: &buf, binary: false}

	token := UID{First: 0, Second: 0}
	// Body larger than 64 bytes — should be truncated in text output
	body := bytes.Repeat([]byte{0xAA}, 100)
	wl.log('S', token, body)

	output := buf.String()
	g.Expect(output).To(gomega.ContainSubstring("more bytes"))
	g.Expect(output).To(gomega.ContainSubstring("len=100"))
	// Should only show 64 bytes of hex
	g.Expect(strings.Count(output, "aa")).To(gomega.Equal(64))
}
