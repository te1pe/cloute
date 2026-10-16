package ws

import (
	"encoding/json"
	"fmt"
	"time"

	"cloute/internal/crdt/wire"
	"cloute/internal/doc"
)

// The wire format is specified in api/protocol.md.

const protocolVersion = 1

// Message types.
const (
	typeHello    = "hello"
	typeWelcome  = "welcome"
	typeOps      = "ops"
	typeAck      = "ack"
	typeError    = "error"
	typePresence = "presence"
	typeComment  = "comment"
	typeComments = "comments"
	typeResolve  = "resolve"
)

// Error codes sent in error messages.
const (
	codeUnsupportedVersion = "unsupported_version"
	codeBadRequest         = "bad_request"
	codeInvalidOp          = "invalid_op"
	codeUnauthorized       = "unauthorized" // no valid session cookie
	codeForbidden          = "forbidden"    // no role in the document, or the role does not allow the action
)

// protocolError is a client mistake reported to the client before closing.
type protocolError struct {
	code    string
	message string
}

func (e *protocolError) Error() string {
	return e.code + ": " + e.message
}

func badRequest(format string, args ...any) *protocolError {
	return &protocolError{code: codeBadRequest, message: fmt.Sprintf(format, args...)}
}

type envelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

type helloMessage struct {
	envelope
	DocID   string    `json:"docId"`
	Replica uint32    `json:"replica,omitempty"` // resume this replica after a reconnect
	LastSeq uint64    `json:"lastSeq,omitempty"`
	Pending []wire.Op `json:"pending,omitempty"`
}

// welcomeMessage carries either Snapshot or Ops (catch-up after LastSeq;
// possibly an empty list), never both.
type welcomeMessage struct {
	envelope
	Replica  uint32         `json:"replica"`
	Role     string         `json:"role"`
	Seq      uint64         `json:"seq"`
	Snapshot *wire.Snapshot `json:"snapshot,omitempty"`
	Ops      []wire.Op      `json:"ops,omitzero"`
}

// opsMessage is sent in both directions; Seq is set only by the server.
type opsMessage struct {
	envelope
	Seq uint64    `json:"seq,omitempty"`
	Ops []wire.Op `json:"ops"`
}

type ackMessage struct {
	envelope
	Seq uint64 `json:"seq"`
}

type cursorJSON struct {
	Anchor wire.ID `json:"anchor"`
	Head   wire.ID `json:"head"`
}

type userJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// presenceMessage: from a client, only Cursor; from the server, whose cursor
// it is. A null cursor from the server means the client left.
type presenceMessage struct {
	envelope
	Replica uint32      `json:"replica,omitempty"`
	User    *userJSON   `json:"user,omitempty"`
	Cursor  *cursorJSON `json:"cursor"`
}

// newCommentMessage is a client's comment on the characters From..To.
type newCommentMessage struct {
	envelope
	From wire.ID `json:"from"`
	To   wire.ID `json:"to"`
	Text string  `json:"text"`
}

type resolveMessage struct {
	envelope
	ID int64 `json:"id"`
}

type commentJSON struct {
	ID        int64     `json:"id"`
	From      wire.ID   `json:"from"`
	To        wire.ID   `json:"to"`
	Text      string    `json:"text"`
	Author    userJSON  `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	Resolved  bool      `json:"resolved"`
}

type commentMessage struct {
	envelope
	Comment commentJSON `json:"comment"`
}

type commentsMessage struct {
	envelope
	Comments []commentJSON `json:"comments"`
}

func encodeComment(c doc.Comment) commentJSON {
	return commentJSON{
		ID: c.ID, From: wire.EncodeID(c.From), To: wire.EncodeID(c.To), Text: c.Body,
		Author: userJSON{ID: c.AuthorID, Name: c.AuthorName}, CreatedAt: c.CreatedAt, Resolved: c.Resolved,
	}
}

func encodePresence(p doc.Presence) presenceMessage {
	m := presenceMessage{envelope: header(typePresence), Replica: p.Replica}
	if p.Cursor != nil {
		m.User = &userJSON{ID: p.UserID, Name: p.UserName}
		m.Cursor = &cursorJSON{Anchor: wire.EncodeID(p.Cursor.Anchor), Head: wire.EncodeID(p.Cursor.Head)}
	}
	return m
}

type errorMessage struct {
	envelope
	Code    string `json:"code"`
	Message string `json:"message"`
}

func header(typ string) envelope {
	return envelope{V: protocolVersion, Type: typ}
}

// decodeClientMessage parses a message from a client and returns
// *helloMessage, *opsMessage, *presenceMessage, *newCommentMessage or
// *resolveMessage.
func decodeClientMessage(data []byte) (any, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, badRequest("malformed JSON: %v", err)
	}
	if env.V != protocolVersion {
		return nil, &protocolError{
			code:    codeUnsupportedVersion,
			message: fmt.Sprintf("protocol version %d is not supported, use %d", env.V, protocolVersion),
		}
	}

	var msg any
	switch env.Type {
	case typeHello:
		msg = &helloMessage{}
	case typeOps:
		msg = &opsMessage{}
	case typePresence:
		msg = &presenceMessage{}
	case typeComment:
		msg = &newCommentMessage{}
	case typeResolve:
		msg = &resolveMessage{}
	default:
		return nil, badRequest("unknown message type %q", env.Type)
	}
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, badRequest("malformed %s message: %v", env.Type, err)
	}
	return msg, nil
}
