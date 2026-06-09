package terminal

// SSM Session Manager datachannel binary protocol.
//
// AWS ECS Exec returns a StreamUrl + TokenValue. The StreamUrl is a WebSocket
// endpoint that speaks a binary framing protocol used by the SSM agent. This file
// implements the minimal subset needed to proxy a shell session:
//
//   open_data_channel  — authenticate and open the channel (sent first)
//   input_stream_data  — carry stdin bytes from the browser to the container
//   output_stream_data — carry stdout/stderr bytes from the container to the browser
//   acknowledge        — ACK every output message (required to prevent backpressure stall)
//   channel_closed     — server notifies that the session ended
//
// Header layout (120 bytes total, all fields big-endian):
//
//	[0:4]   HeaderLength = 116
//	[4:36]  MessageType  (32 bytes, space-padded)
//	[36:40] SchemaVersion = 1
//	[40:48] CreatedDate  (uint64 epoch ms)
//	[48:56] SequenceNumber (int64)
//	[56:64] Flags        (uint64)
//	[64:80] MessageId    (16-byte UUID)
//	[80:112] PayloadDigest (SHA-256)
//	[112:116] PayloadType (uint32) — 1 = data
//	[116:120] PayloadLength (uint32)
//	[120:] Payload

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	headerLengthValue uint32 = 116
	schemaVersionValue uint32 = 1
	payloadTypeData   uint32 = 1
	payloadTypeSize   uint32 = 17 // PTY resize

	msgTypeOpen   = "open_data_channel"
	msgTypeInput  = "input_stream_data"
	msgTypeOutput = "output_stream_data"
	msgTypeAck    = "acknowledge"
	msgTypeClosed = "channel_closed"
)

type clientMessage struct {
	headerLength   uint32
	messageType    string
	schemaVersion  uint32
	createdDate    uint64
	sequenceNumber int64
	flags          uint64
	messageID      uuid.UUID
	payloadDigest  [32]byte
	payloadType    uint32
	payloadLength  uint32
	payload        []byte
}

type acknowledgePayload struct {
	AcknowledgedMessageType           string `json:"AcknowledgedMessageType"`
	AcknowledgedMessageID             string `json:"AcknowledgedMessageId"`
	AcknowledgedMessageSchemaVersion  uint32 `json:"AcknowledgedMessageSchemaVersion"`
	AcknowledgedMessageSequenceNumber int64  `json:"AcknowledgedMessageSequenceNumber"`
	IsBufferFull                      bool   `json:"IsBufferFull"`
}

type resizePayload struct {
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

func newMessage(msgType string, seqNum int64, payloadType uint32, payload []byte) clientMessage {
	digest := sha256.Sum256(payload)
	return clientMessage{
		headerLength:   headerLengthValue,
		messageType:    msgType,
		schemaVersion:  schemaVersionValue,
		createdDate:    uint64(time.Now().UnixMilli()),
		sequenceNumber: seqNum,
		messageID:      uuid.New(),
		payloadDigest:  digest,
		payloadType:    payloadType,
		payloadLength:  uint32(len(payload)),
		payload:        payload,
	}
}

func serialize(msg clientMessage) ([]byte, error) {
	buf := &bytes.Buffer{}

	binary.Write(buf, binary.BigEndian, msg.headerLength)

	msgTypeBytes := make([]byte, 32)
	copy(msgTypeBytes, []byte(msg.messageType))
	for i := len(msg.messageType); i < 32; i++ {
		msgTypeBytes[i] = ' '
	}
	buf.Write(msgTypeBytes)

	binary.Write(buf, binary.BigEndian, msg.schemaVersion)
	binary.Write(buf, binary.BigEndian, msg.createdDate)
	binary.Write(buf, binary.BigEndian, msg.sequenceNumber)
	binary.Write(buf, binary.BigEndian, msg.flags)
	buf.Write(msg.messageID[:])
	buf.Write(msg.payloadDigest[:])
	binary.Write(buf, binary.BigEndian, msg.payloadType)
	binary.Write(buf, binary.BigEndian, msg.payloadLength)
	buf.Write(msg.payload)

	return buf.Bytes(), nil
}

func deserialize(data []byte) (clientMessage, error) {
	if len(data) < 120 {
		return clientMessage{}, fmt.Errorf("frame too short: %d bytes", len(data))
	}

	r := bytes.NewReader(data)
	var msg clientMessage

	binary.Read(r, binary.BigEndian, &msg.headerLength)

	var typeBuf [32]byte
	r.Read(typeBuf[:])
	msg.messageType = strings.TrimRight(string(typeBuf[:]), " ")

	binary.Read(r, binary.BigEndian, &msg.schemaVersion)
	binary.Read(r, binary.BigEndian, &msg.createdDate)
	binary.Read(r, binary.BigEndian, &msg.sequenceNumber)
	binary.Read(r, binary.BigEndian, &msg.flags)

	var idBuf [16]byte
	r.Read(idBuf[:])
	msg.messageID = uuid.UUID(idBuf)

	r.Read(msg.payloadDigest[:])
	binary.Read(r, binary.BigEndian, &msg.payloadType)
	binary.Read(r, binary.BigEndian, &msg.payloadLength)

	if msg.payloadLength > 0 {
		msg.payload = make([]byte, msg.payloadLength)
		if _, err := r.Read(msg.payload); err != nil {
			return msg, fmt.Errorf("failed to read payload: %w", err)
		}
	}

	return msg, nil
}

func buildOpenMessage(tokenValue string) ([]byte, error) {
	payload, err := json.Marshal(map[string]string{"TokenValue": tokenValue})
	if err != nil {
		return nil, err
	}
	return serialize(newMessage(msgTypeOpen, 0, payloadTypeData, payload))
}

func buildInputMessage(seqNum int64, data []byte) ([]byte, error) {
	return serialize(newMessage(msgTypeInput, seqNum, payloadTypeData, data))
}

func buildResizeMessage(seqNum int64, cols, rows uint32) ([]byte, error) {
	payload, err := json.Marshal(resizePayload{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return serialize(newMessage(msgTypeInput, seqNum, payloadTypeSize, payload))
}

func buildAckMessage(seqNum int64, acked clientMessage) ([]byte, error) {
	payload, err := json.Marshal(acknowledgePayload{
		AcknowledgedMessageType:           acked.messageType,
		AcknowledgedMessageID:             acked.messageID.String(),
		AcknowledgedMessageSchemaVersion:  acked.schemaVersion,
		AcknowledgedMessageSequenceNumber: acked.sequenceNumber,
	})
	if err != nil {
		return nil, err
	}
	return serialize(newMessage(msgTypeAck, seqNum, payloadTypeData, payload))
}
