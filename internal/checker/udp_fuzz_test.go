package checker

import "testing"

func FuzzParseUDPPacket(f *testing.F) {
	f.Add([]byte{0x00, 0x00, 0x00, 0x01})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0x00})
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03})

	f.Fuzz(func(t *testing.T, data []byte) {
		seq, ok := ParseUDPPacket(data)
		if len(data) < 4 {
			if ok {
				t.Error("expected ok=false for short data")
			}
			if seq != 0 {
				t.Error("expected seq=0 for short data")
			}
		} else if !ok {
			t.Error("expected ok=true for data >= 4 bytes")
		}
	})
}
