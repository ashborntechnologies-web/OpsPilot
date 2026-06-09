package terminal

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSerializeDeserialize_Roundtrip(t *testing.T) {
	payload := []byte("hello terminal")
	msg := newMessage(msgTypeInput, 7, payloadTypeData, payload)

	raw, err := serialize(msg)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(raw), 120)

	got, err := deserialize(raw)
	require.NoError(t, err)

	assert.Equal(t, msgTypeInput, got.messageType)
	assert.Equal(t, int64(7), got.sequenceNumber)
	assert.Equal(t, payloadTypeData, got.payloadType)
	assert.Equal(t, payload, got.payload)
	assert.Equal(t, uint32(len(payload)), got.payloadLength)
	assert.Equal(t, headerLengthValue, got.headerLength)
	assert.Equal(t, schemaVersionValue, got.schemaVersion)
	assert.Equal(t, msg.messageID, got.messageID)
	assert.Equal(t, msg.payloadDigest, got.payloadDigest)
}

func TestDeserialize_TooShort(t *testing.T) {
	_, err := deserialize(make([]byte, 50))
	assert.ErrorContains(t, err, "frame too short")
}

func TestDeserialize_ExactMinimum(t *testing.T) {
	// 120-byte frame with zero payload — should not error
	raw := make([]byte, 120)
	// Set headerLength field (bytes 0-3) to 116
	raw[0], raw[1], raw[2], raw[3] = 0, 0, 0, 116
	_, err := deserialize(raw)
	assert.NoError(t, err)
}

func TestBuildOpenMessage_ContainsToken(t *testing.T) {
	token := "test-token-12345"
	raw, err := buildOpenMessage(token)
	require.NoError(t, err)

	msg, err := deserialize(raw)
	require.NoError(t, err)

	assert.Equal(t, msgTypeOpen, msg.messageType)
	assert.Equal(t, int64(0), msg.sequenceNumber)
	assert.Equal(t, payloadTypeData, msg.payloadType)

	var body map[string]string
	require.NoError(t, json.Unmarshal(msg.payload, &body))
	assert.Equal(t, token, body["TokenValue"])
}

func TestBuildInputMessage(t *testing.T) {
	data := []byte("ls -la\n")
	raw, err := buildInputMessage(3, data)
	require.NoError(t, err)

	msg, err := deserialize(raw)
	require.NoError(t, err)

	assert.Equal(t, msgTypeInput, msg.messageType)
	assert.Equal(t, int64(3), msg.sequenceNumber)
	assert.Equal(t, payloadTypeData, msg.payloadType)
	assert.Equal(t, data, msg.payload)
}

func TestBuildResizeMessage_PayloadType(t *testing.T) {
	raw, err := buildResizeMessage(1, 120, 40)
	require.NoError(t, err)

	msg, err := deserialize(raw)
	require.NoError(t, err)

	// Resize messages use PayloadType 17
	assert.Equal(t, payloadTypeSize, msg.payloadType)

	var rp resizePayload
	require.NoError(t, json.Unmarshal(msg.payload, &rp))
	assert.Equal(t, uint32(120), rp.Cols)
	assert.Equal(t, uint32(40), rp.Rows)
}

func TestBuildAckMessage(t *testing.T) {
	// Build a fake output message to ACK
	output := newMessage(msgTypeOutput, 5, payloadTypeData, []byte("output data"))
	raw, err := buildAckMessage(6, output)
	require.NoError(t, err)

	msg, err := deserialize(raw)
	require.NoError(t, err)

	assert.Equal(t, msgTypeAck, msg.messageType)

	var ack acknowledgePayload
	require.NoError(t, json.Unmarshal(msg.payload, &ack))
	assert.Equal(t, msgTypeOutput, ack.AcknowledgedMessageType)
	assert.Equal(t, output.messageID.String(), ack.AcknowledgedMessageID)
	assert.Equal(t, int64(5), ack.AcknowledgedMessageSequenceNumber)
}

func TestMessageTypeSpacePadded(t *testing.T) {
	// SSM protocol requires message type to be space-padded to exactly 32 bytes
	msg := newMessage("short", 0, payloadTypeData, []byte{})
	raw, err := serialize(msg)
	require.NoError(t, err)
	// Bytes [4:36] are the type field
	typeBytes := raw[4:36]
	assert.Equal(t, byte('s'), typeBytes[0]) // 's' from "short"
	assert.Equal(t, byte('h'), typeBytes[1]) // 'h' from "short"
	// Padding
	assert.Equal(t, byte(' '), typeBytes[10])
	assert.Equal(t, byte(' '), typeBytes[31])
}
