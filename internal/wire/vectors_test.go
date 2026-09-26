package wire

import (
	"bytes"
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"

	fleetv1 "fleet/gen/fleetv1"
)

// Framing vectors published in docs/PROTOCOL.md ("Test vectors").
func TestFrameVectors(t *testing.T) {
	tests := []struct {
		name string
		msg  proto.Message
		want string
	}{
		{
			"ping id 1",
			&fleetv1.ClientMessage{Id: 1, Msg: &fleetv1.ClientMessage_Ping{Ping: &fleetv1.Ping{}}},
			"00000004" + "08016200",
		},
		{
			"terminal input id 0",
			&fleetv1.ClientMessage{Msg: &fleetv1.ClientMessage_TerminalInput{TerminalInput: &fleetv1.TerminalInput{
				AgentId: "a1b2c3", Data: []byte("ls\r"),
			}}},
			"00000010" + "ca020d0a0661316232633312036c730d",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tt.msg); err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(buf.Bytes()); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
			got := &fleetv1.ClientMessage{}
			if err := ReadFrame(&buf, got); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, tt.msg) {
				t.Errorf("round trip: got %v, want %v", got, tt.msg)
			}
		})
	}
}
