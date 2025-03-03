package request

import (
	"time"

	"github.com/gavv/monotime"

	"github.com/grafana/beyla/pkg/internal/svc"
)

type EventType int

// The following consts need to coincide with some C identifiers:
// EVENT_HTTP_REQUEST, EVENT_GRPC_REQUEST, EVENT_HTTP_CLIENT, EVENT_GRPC_CLIENT, EVENT_SQL_CLIENT
const (
	EventTypeHTTP EventType = iota + 1
	EventTypeGRPC
	EventTypeHTTPClient
	EventTypeGRPCClient
	EventTypeSQLClient
)

type converter struct {
	clock     func() time.Time
	monoClock func() time.Duration
}

var clocks = converter{monoClock: monotime.Now, clock: time.Now}

// Span contains the information being submitted by the following nodes in the graph.
// It enables comfortable handling of data from Go.
type Span struct {
	Type          EventType
	ID            uint64
	Method        string
	Path          string
	Route         string
	Peer          string
	Host          string
	HostPort      int
	Status        int
	ContentLength int64
	RequestStart  int64
	Start         int64
	End           int64
	ServiceID     svc.ID
	Metadata      map[string]string
	Traceparent   string
}

func (s *Span) Inside(parent *Span) bool {
	return s.RequestStart >= parent.RequestStart && s.End <= parent.End
}

type Timings struct {
	RequestStart time.Time
	Start        time.Time
	End          time.Time
}

func (s *Span) Timings() Timings {
	now := clocks.clock()
	monoNow := clocks.monoClock()
	startDelta := monoNow - time.Duration(s.Start)
	endDelta := monoNow - time.Duration(s.End)
	goStartDelta := monoNow - time.Duration(s.RequestStart)

	return Timings{
		RequestStart: now.Add(-goStartDelta),
		Start:        now.Add(-startDelta),
		End:          now.Add(-endDelta),
	}
}
