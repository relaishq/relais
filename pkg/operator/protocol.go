// Package operator defines the versioned, resource-neutral operator wire contract.
// Payloads are private adapter data; only Result and Trace are public artifacts.
package operator

import (
	"encoding/json"
	"time"
)

// HostSubmissionMinimum is the admission floor for host.execute, including
// observations. Adapters recheck it before submitting a remote host command.
const HostSubmissionMinimum = 30 * time.Second

const ProtocolVersion = 1
const MaxFrameBytes = 256 << 10
const MaxScriptBytes = 64 << 10
const MaxHostOutputBytes = 23999
const MaxBlobBytes = 64 << 20

type Certainty string

const (
	Succeeded Certainty = "succeeded"
	Failed    Certainty = "failed"
	Unknown   Certainty = "unknown"
)

func (c Certainty) Valid() bool { return c == Succeeded || c == Failed || c == Unknown }

type Operation string

const (
	HostExecute Operation = "host.execute"
	// HostReadBatch is optional. It groups four independent inventory or clock
	// reads; adapters must join every result and must never retry a child.
	HostReadBatch Operation = "host.read-batch"
	CallerReady   Operation = "caller.ready"
	CallerStart   Operation = "caller.start"
	CallerStop    Operation = "caller.stop"
	MailboxRead   Operation = "mailbox.read"
	MailboxCreate Operation = "mailbox.create"
	StoreObserve  Operation = "store.observe"
	StoreFailover Operation = "store.failover"
	// CallerAdmission is an optional negotiation marker. Its effects use the
	// existing caller and mailbox operations with the admission-boundary phase.
	CallerAdmission Operation = "caller.admission"
)

// IDs and Unix milliseconds are decimal strings, avoiding floating-point loss.
type Request struct {
	ID             string          `json:"id"`
	Operation      Operation       `json:"operation"`
	Target         string          `json:"target"`
	Action         string          `json:"action,omitempty"`
	Args           json.RawMessage `json:"args,omitempty"`
	Script         []byte          `json:"script,omitempty"`
	DeadlineUnixMS string          `json:"deadlineUnixMs"`
	RemainingMS    int64           `json:"remainingMs"`
}

type Reply struct {
	UncertaintyRule string    `json:"uncertaintyRule,omitempty"`
	ID              string    `json:"id"`
	Transport       Certainty `json:"transport"`
	ExitCode        *int      `json:"exitCode,omitempty"`
	// NotSubmitted proves a refusal before any submission, not an arbitrary error.
	NotSubmitted bool            `json:"notSubmitted,omitempty"`
	RefusalCode  string          `json:"refusalCode,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	Blob         *Blob           `json:"blob,omitempty"`
}

// HostOutput is the bounded stdout of one host.execute completion. Exit status
// and transport certainty stay on Reply; this envelope never supplies a verdict.
type HostOutput struct {
	Stdout string `json:"stdout"`
}

type Blob struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Init struct {
	Scenario      Scenario    `json:"scenario"`
	ArmedAtUnixMS string      `json:"armedAtUnixMs"`
	EndUnixMS     string      `json:"endUnixMs"`
	Capabilities  []Operation `json:"capabilities"`
}

type Hello struct {
	Protocol       int    `json:"protocol"`
	Version        string `json:"version"`
	Commit         string `json:"commit"`
	ScenarioSchema int    `json:"scenarioSchema"`
	ResultSchema   int    `json:"resultSchema"`
}

// A frame has exactly one body selected by Type. Session is opaque local identity.
type Frame struct {
	Type    string   `json:"type"`
	Session string   `json:"session,omitempty"`
	Hello   *Hello   `json:"hello,omitempty"`
	Init    *Init    `json:"init,omitempty"`
	Request *Request `json:"request,omitempty"`
	Result  *Reply   `json:"result,omitempty"`
	Trace   *Trace   `json:"trace,omitempty"`
	Final   *Result  `json:"final,omitempty"`
	Code    string   `json:"code,omitempty"`
}

// HostReadBatchArgs contains one fixed pure observation for each host. The
// outer request supplies the action and immutable deadline for every child.
type HostReadBatchArgs struct {
	Reads []HostRead `json:"reads"`
}
type HostRead struct {
	Target string `json:"target"`
	Script []byte `json:"script"`
}

// Child reply IDs are host slots, in request order. Each child retains its own
// transport certainty, exit status and bounded HostOutput. Blobs are forbidden.
type HostReadBatchOutput struct {
	Replies []Reply `json:"replies"`
}
