//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"github.com/mariomac/guara/pkg/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/beyla/test/integration/components/jaeger"
	grpcclient "github.com/grafana/beyla/test/integration/components/testserver/grpc/client"
)

func testHTTPTracesNoTraceID(t *testing.T) {
	testHTTPTracesCommon(t, false, 200)
}

func testHTTPTraces(t *testing.T) {
	testHTTPTracesCommon(t, true, 500)
}

func testHTTPTracesCommon(t *testing.T, doTraceID bool, httpCode int) {
	var traceID string
	var parentID string

	slug := "create-trace"
	if doTraceID {
		slug = "create-trace-with-id"
		// Add and check for specific trace ID
		traceID = createTraceID()
		parentID = createParentID()
		traceparent := createTraceparent(traceID, parentID)
		doHTTPGetWithTraceparent(t, fmt.Sprintf("%s/%s?delay=10ms&status=%d", instrumentedServiceStdURL, slug, httpCode), httpCode, traceparent)
	} else {
		doHTTPGet(t, fmt.Sprintf("%s/%s?delay=10ms&status=%d", instrumentedServiceStdURL, slug, httpCode), httpCode)
	}

	var trace jaeger.Trace
	test.Eventually(t, testTimeout, func(t require.TestingT) {
		resp, err := http.Get(jaegerQueryURL + "?service=testserver&operation=GET%20%2F" + slug)
		require.NoError(t, err)
		if resp == nil {
			return
		}
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var tq jaeger.TracesQuery
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&tq))
		traces := tq.FindBySpan(jaeger.Tag{Key: "http.target", Type: "string", Value: "/" + slug})
		require.Len(t, traces, 1)
		trace = traces[0]
	}, test.Interval(100*time.Millisecond))

	// Check the information of the parent span
	res := trace.FindByOperationName("GET /" + slug)
	require.Len(t, res, 1)
	parent := res[0]
	require.NotEmpty(t, parent.TraceID)
	if doTraceID {
		require.Equal(t, traceID, parent.TraceID)
		// Validate that "parent" is a CHILD_OF the traceparent's "parent-id"
		childOfPID := trace.ChildrenOf(parentID)
		require.Len(t, childOfPID, 1)
	}
	require.NotEmpty(t, parent.SpanID)
	// check duration is at least 10ms
	assert.Less(t, (10 * time.Millisecond).Microseconds(), parent.Duration)
	// check span attributes
	sd := parent.Diff(
		jaeger.Tag{Key: "http.method", Type: "string", Value: "GET"},
		jaeger.Tag{Key: "http.status_code", Type: "int64", Value: float64(httpCode)},
		jaeger.Tag{Key: "http.target", Type: "string", Value: "/" + slug},
		jaeger.Tag{Key: "net.host.port", Type: "int64", Value: float64(8080)},
		jaeger.Tag{Key: "http.route", Type: "string", Value: "/" + slug},
		jaeger.Tag{Key: "span.kind", Type: "string", Value: "server"},
	)
	assert.Empty(t, sd, sd.String())

	if httpCode >= 500 {
		sd := parent.Diff(
			jaeger.Tag{Key: "otel.status_code", Type: "string", Value: "ERROR"},
		)
		assert.Empty(t, sd, sd.String())
	}

	// Check the information of the "in queue" span
	res = trace.FindByOperationName("in queue")
	require.Len(t, res, 1)
	queue := res[0]
	// Check parenthood
	p, ok := trace.ParentOf(&queue)
	require.True(t, ok)
	assert.Equal(t, parent.TraceID, p.TraceID)
	assert.Equal(t, parent.SpanID, p.SpanID)
	// check reasonable start and end times
	assert.GreaterOrEqual(t, queue.StartTime, parent.StartTime)
	assert.LessOrEqual(t,
		queue.StartTime+queue.Duration,
		parent.StartTime+parent.Duration+1) // adding 1 to tolerate inaccuracies from rounding from ns to ms
	// check span attributes
	// check span attributes
	sd = queue.Diff(
		jaeger.Tag{Key: "span.kind", Type: "string", Value: "internal"},
	)
	assert.Empty(t, sd, sd.String())

	// Check the information of the "processing" span
	res = trace.FindByOperationName("processing")
	require.Len(t, res, 1)
	processing := res[0]
	// Check parenthood
	p, ok = trace.ParentOf(&queue)
	require.True(t, ok)
	assert.Equal(t, parent.TraceID, p.TraceID)
	assert.Equal(t, parent.SpanID, p.SpanID)
	// check reasonable start and end times
	assert.GreaterOrEqual(t, processing.StartTime, queue.StartTime+queue.Duration)
	assert.LessOrEqual(t,
		processing.StartTime+processing.Duration,
		parent.StartTime+parent.Duration+1)
	sd = queue.Diff(
		jaeger.Tag{Key: "span.kind", Type: "string", Value: "internal"},
	)
	assert.Empty(t, sd, sd.String())

	// check process ID
	require.Contains(t, trace.Processes, parent.ProcessID)
	assert.Equal(t, parent.ProcessID, queue.ProcessID)
	assert.Equal(t, parent.ProcessID, processing.ProcessID)
	process := trace.Processes[parent.ProcessID]
	assert.Equal(t, "testserver", process.ServiceName)
	jaeger.Diff([]jaeger.Tag{
		{Key: "otel.library.name", Type: "string", Value: "github.com/grafana/beyla"},
		{Key: "telemetry.sdk.language", Type: "string", Value: "go"},
		{Key: "service.namespace", Type: "string", Value: "integration-test"},
	}, process.Tags)
	assert.Empty(t, sd, sd.String())
}

func testHTTPTracesBadTraceparent(t *testing.T) {
	slugToParent := map[string]string{
		// Valid traceparent example:
		//		valid: "00-5fe865607da112abd799ea8108c38bcb-4c59e9a913c480a3-01"
		// Examples of INVALID traceIDs in traceparent:  Note: eBPF rejects when len != 55
		"invalid-trace-id1": "00-Zfe865607da112abd799ea8108c38bcb-4c59e9a913c480a3-01",
		"invalid-trace-id2": "00-5fe865607da112abd799ea8108c38bcL-4c59e9a913c480a3-01",
		"invalid-trace-id3": "00-5fe865607Ra112abd799ea8108c38bcb-4c59e9a913c480a3-01",
		"invalid-trace-id4": "00-0x5fe865607da112abd799ea8108c3cb-4c59e9a913c480a3-01",
		"invalid-trace-id5": "00-5FE865607DA112ABD799EA8108C38BCB-4c59e9a913c480a3-01",
		// For parent test, traceID portion must be different each time
		"invalid-parent-id1": "00-11111111111111111111111111111111-Zc59e9a913c480a3-01",
		"invalid-parent-id2": "00-22222222222222222222222222222222-4C59E9A913C480A3-01",
		"invalid-parent-id3": "00-33333333333333333333333333333333-4c59e9aW13c480a3-01",
		"invalid-parent-id4": "00-44444444444444444444444444444444-4c59e9a9-3c480a3-01",
		"invalid-parent-id5": "00-55555555555555555555555555555555-0x59e9a913c480a3-01",
		"invalid-flags-1":    "00-176716bec4d4c0e85df0d39dd70a2b62-c7fe2560276e9ba0-0x",
		"invalid-flags-2":    "00-b97fd2bfb304550fd85c33fdfc821f29-dfca787aa452fcdb-No",
		"not-sampled-flag-1": "00-48ebacb3fe3ebaa5df61f611dda9a094-c1c831f7da1a9309-00",
		"not-sampled-flag-2": "00-d9e4d0f83479f891815e33af16175af8-eaff68618edf4279-f0",
		"not-sampled-flag-3": "00-be8faab0d17fe5424d142a3b356a5d35-d52a68b9f0cf468e-12",
	}
	for slug, traceparent := range slugToParent {
		t.Log("Testing bad traceid. traceparent:", traceparent, "slug:", slug)
		doHTTPGetWithTraceparent(t, instrumentedServiceStdURL+"/"+slug+"?delay=10ms", 200, traceparent)

		var trace jaeger.Trace
		negativeTest := strings.Contains(slug, "flag")
		test.Eventually(t, testTimeout, func(t require.TestingT) {
			if negativeTest {
				// Give time when we're ensuring that a trace is NOT generated
				time.Sleep(min(10, testTimeout/2) * time.Second)
			}
			resp, err := http.Get(jaegerQueryURL + "?service=testserver&operation=GET%20%2F" + slug)
			require.NoError(t, err)
			if resp == nil {
				return
			}
			require.Equal(t, http.StatusOK, resp.StatusCode)
			var tq jaeger.TracesQuery
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&tq))
			traces := tq.FindBySpan(jaeger.Tag{Key: "http.target", Type: "string", Value: "/" + slug})
			if negativeTest {
				require.Len(t, traces, 0)
			} else {
				require.Len(t, traces, 1)
				trace = traces[0]
			}
		}, test.Interval(100*time.Millisecond))

		if negativeTest {
			continue
		}
		// Check the information of the parent span
		res := trace.FindByOperationName("GET /" + slug)
		require.Len(t, res, 1)
		parent := res[0]
		require.NotEmpty(t, parent.TraceID)
		if strings.Contains(slug, "trace-id") {
			require.NotEqual(t, traceparent[3:35], parent.TraceID)
		} else if strings.Contains(slug, "parent-id") {
			children := trace.ChildrenOf(traceparent[36:52])
			require.Equal(t, len(children), 0)
		}
	}
}

func testGRPCTraces(t *testing.T) {
	testGRPCTracesForServiceName(t, "testserver")
}

func testGRPCTracesForServiceName(t *testing.T, svcName string) {
	require.Error(t, grpcclient.Debug(10*time.Millisecond, true)) // this call doesn't add anything, the Go SDK will generate traceID and contextID

	var trace jaeger.Trace
	test.Eventually(t, testTimeout, func(t require.TestingT) {
		resp, err := http.Get(jaegerQueryURL + "?service=" + svcName + "&operation=%2Frouteguide.RouteGuide%2FDebug")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var tq jaeger.TracesQuery
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&tq))
		traces := tq.FindBySpan(jaeger.Tag{Key: "rpc.method", Type: "string", Value: "/routeguide.RouteGuide/Debug"})
		require.Len(t, traces, 1)
		trace = traces[0]
	}, test.Interval(100*time.Millisecond))

	// Check the information of the parent span
	res := trace.FindByOperationName("/routeguide.RouteGuide/Debug")
	require.Len(t, res, 1)
	parent := res[0]
	require.NotEmpty(t, parent.TraceID)
	require.NotEmpty(t, parent.SpanID)
	// check duration is at least 10ms (10,000 microseconds)
	assert.Less(t, (10 * time.Millisecond).Microseconds(), parent.Duration)
	// check span attributes
	sd := parent.Diff(
		jaeger.Tag{Key: "net.host.port", Type: "int64", Value: float64(50051)},
		jaeger.Tag{Key: "rpc.grpc.status_code", Type: "int64", Value: float64(2)},
		jaeger.Tag{Key: "rpc.method", Type: "string", Value: "/routeguide.RouteGuide/Debug"},
		jaeger.Tag{Key: "rpc.system", Type: "string", Value: "grpc"},
		jaeger.Tag{Key: "span.kind", Type: "string", Value: "server"},
		jaeger.Tag{Key: "service.name", Type: "string", Value: svcName},
	)
	assert.Empty(t, sd, sd.String())

	// Check the information of the "in queue" span
	res = trace.FindByOperationName("in queue")
	require.Len(t, res, 1)
	queue := res[0]
	// Check parenthood
	p, ok := trace.ParentOf(&queue)
	require.True(t, ok)
	assert.Equal(t, parent.TraceID, p.TraceID)
	assert.Equal(t, parent.SpanID, p.SpanID)
	// check reasonable start and end times
	assert.GreaterOrEqual(t, queue.StartTime, parent.StartTime)
	assert.LessOrEqual(t,
		queue.StartTime+queue.Duration,
		parent.StartTime+parent.Duration+1) // adding 1 to tolerate inaccuracies from rounding from ns to ms
	// check span attributes
	sd = queue.Diff(
		jaeger.Tag{Key: "span.kind", Type: "string", Value: "internal"},
	)
	assert.Empty(t, sd, sd.String())

	// Check the information of the "processing" span
	res = trace.FindByOperationName("processing")
	require.Len(t, res, 1)
	processing := res[0]
	// Check parenthood
	p, ok = trace.ParentOf(&queue)
	require.True(t, ok)
	assert.Equal(t, parent.TraceID, p.TraceID)
	require.False(t, strings.HasSuffix(parent.TraceID, "0000000000000000")) // the Debug call doesn't add any traceparent to the request header, the traceID is auto-generated won't look like this
	assert.Equal(t, parent.SpanID, p.SpanID)
	// check reasonable start and end times
	assert.GreaterOrEqual(t, processing.StartTime, queue.StartTime+queue.Duration)
	assert.LessOrEqual(t, processing.StartTime+processing.Duration, parent.StartTime+parent.Duration+1)
	// check span attributes
	sd = queue.Diff(
		jaeger.Tag{Key: "span.kind", Type: "string", Value: "internal"},
	)
	assert.Empty(t, sd, sd.String())

	// check process ID
	require.Contains(t, trace.Processes, parent.ProcessID)
	assert.Equal(t, parent.ProcessID, queue.ProcessID)
	assert.Equal(t, parent.ProcessID, processing.ProcessID)
	process := trace.Processes[parent.ProcessID]
	assert.Equal(t, svcName, process.ServiceName)
	jaeger.Diff([]jaeger.Tag{
		{Key: "otel.library.name", Type: "string", Value: "github.com/grafana/beyla"},
		{Key: "telemetry.sdk.language", Type: "string", Value: "go"},
		{Key: "service.namespace", Type: "string", Value: "integration-test"},
	}, process.Tags)
	assert.Empty(t, sd, sd.String())

	require.NoError(t, grpcclient.List()) // this call adds traceparent manually to the headers, simulates existing traceparent

	test.Eventually(t, testTimeout, func(t require.TestingT) {
		resp, err := http.Get(jaegerQueryURL + "?service=" + svcName + "&operation=%2Frouteguide.RouteGuide%2FListFeatures")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var tq jaeger.TracesQuery
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&tq))
		traces := tq.FindBySpan(jaeger.Tag{Key: "rpc.method", Type: "string", Value: "/routeguide.RouteGuide/ListFeatures"})
		require.Len(t, traces, 1)
		trace = traces[0]
	}, test.Interval(100*time.Millisecond))

	// Check the information of the parent span
	res = trace.FindByOperationName("/routeguide.RouteGuide/ListFeatures")
	require.Len(t, res, 1)
	parent = res[0]
	require.NotEmpty(t, parent.TraceID)
	require.NotEmpty(t, parent.SpanID)

	/*
	 The code for grpc Ping() generates spans like these:
	 00-000000000000038b0000000000000000-000000000000038b-01

	 The traceID and spanID increase by one in tandem and it loops forever.
	 We check that the traceID has that 16 character 0 suffix and then we
	 use the first 16 characters for looking up by Parent span.

	 Finding a traceID like the custom pattern means that our traceparent
	 extraction in eBPF works.
	*/
	require.NotEmpty(t, parent.TraceID)
	require.True(t, strings.HasSuffix(parent.TraceID, "0000000000000000"))

	pparent := parent.TraceID[:16]
	childOfPID := trace.ChildrenOf(pparent)
	require.Len(t, childOfPID, 1)
	childSpan := childOfPID[0]
	require.Equal(t, childSpan.TraceID, parent.TraceID)
	require.Equal(t, childSpan.SpanID, parent.SpanID)
}

func testHTTPTracesKProbes(t *testing.T) {
	var traceID string
	var parentID string

	// Add and check for specific trace ID
	traceID = createTraceID()
	parentID = createParentID()
	traceparent := createTraceparent(traceID, parentID)
	doHTTPGetWithTraceparent(t, "http://localhost:3031/bye", 200, traceparent)

	var trace jaeger.Trace
	test.Eventually(t, testTimeout, func(t require.TestingT) {
		resp, err := http.Get(jaegerQueryURL + "?service=node&operation=GET%20%2Fbye")
		require.NoError(t, err)
		if resp == nil {
			return
		}
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var tq jaeger.TracesQuery
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&tq))
		traces := tq.FindBySpan(jaeger.Tag{Key: "http.target", Type: "string", Value: "/bye"})
		require.Len(t, traces, 1)
		trace = traces[0]
	}, test.Interval(100*time.Millisecond))

	// Check the information of the parent span
	res := trace.FindByOperationName("GET /bye")
	require.Len(t, res, 1)
	parent := res[0]
	require.NotEmpty(t, parent.TraceID)
	require.Equal(t, traceID, parent.TraceID)
	// Validate that "parent" is a CHILD_OF the traceparent's "parent-id"
	childOfPID := trace.ChildrenOf(parentID)
	require.Len(t, childOfPID, 1)
	require.NotEmpty(t, parent.SpanID)
	// check duration is at least 2us
	assert.Less(t, (2 * time.Microsecond).Microseconds(), parent.Duration)
	// check span attributes
	sd := parent.Diff(
		jaeger.Tag{Key: "http.method", Type: "string", Value: "GET"},
		jaeger.Tag{Key: "http.status_code", Type: "int64", Value: float64(200)},
		jaeger.Tag{Key: "http.target", Type: "string", Value: "/bye"},
		jaeger.Tag{Key: "net.host.port", Type: "int64", Value: float64(3030)},
		jaeger.Tag{Key: "http.route", Type: "string", Value: "/bye"},
		jaeger.Tag{Key: "span.kind", Type: "string", Value: "server"},
	)
	assert.Empty(t, sd, sd.String())

	process := trace.Processes[parent.ProcessID]
	assert.Equal(t, "node", process.ServiceName)
	jaeger.Diff([]jaeger.Tag{
		{Key: "otel.library.name", Type: "string", Value: "github.com/grafana/beyla"},
		{Key: "telemetry.sdk.language", Type: "string", Value: "go"},
		{Key: "service.namespace", Type: "string", Value: "integration-test"},
	}, process.Tags)
	assert.Empty(t, sd, sd.String())
}
