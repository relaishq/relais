package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	CheckpointWrites = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "relais_checkpoint_writes_total", Help: "Checkpoint write attempts by result",
	}, []string{"result"})
	CheckpointEnvelopeEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "relais_checkpoint_envelope_events_total", Help: "Takeovers outside the checkpoint envelope by policy",
	}, []string{"policy"})
	CheckpointAge = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "relais_checkpoint_age_seconds", Help: "Age since successful checkpoint write at crash takeover",
		Buckets: prometheus.DefBuckets,
	})

	EgressReads = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_egress_reads_total",
			Help: "Total number of egress read cycles",
		},
		[]string{"mode"}, // session|track
	)

	EgressEmptyPolls = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "relais_egress_empty_polls_total",
			Help: "Total number of empty polls in egress tailer",
		},
	)

	EgressErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_egress_errors_total",
			Help: "Total number of egress errors",
		},
		[]string{"stage"}, // xread_session|xread_video|xread_audio|poll
	)

	FramesForwarded = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_frames_forwarded_total",
			Help: "Total frames forwarded to PeerConnection",
		},
		[]string{"media"}, // video|audio
	)

	EgressReadDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "relais_egress_read_seconds",
			Help:    "Duration of stream reads",
			Buckets: prometheus.DefBuckets,
		},
	)

	// Redis operation metrics (storage layer)
	RedisOpDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "relais_redis_op_seconds",
			Help:    "Duration of Redis operations",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"op"}, // e.g., pipeline_exec|xread|xread_track|get|set
	)

	RedisErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_redis_errors_total",
			Help: "Total number of Redis operation errors by type",
		},
		[]string{"op", "type"}, // timeout|net|eof|moved|ask|busy|tryagain|other
	)

	RedisRetries = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_redis_retries_total",
			Help: "Total number of Redis operation retries",
		},
		[]string{"op"},
	)

	// Redis Streams Consumer Group observability
	RedisGroupReadOps = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_redis_group_reads_total",
			Help: "Total number of XREADGROUP calls",
		},
		[]string{"stream"}, // session|track
	)

	RedisGroupReadMessages = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_redis_group_read_messages_total",
			Help: "Total number of messages read via XREADGROUP",
		},
		[]string{"stream"}, // session|track
	)

	RedisGroupAcks = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_redis_group_acks_total",
			Help: "Total number of XACK calls",
		},
		[]string{"stream"}, // session|track
	)

	RedisGroupAckMessages = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_redis_group_ack_messages_total",
			Help: "Total number of messages acknowledged via XACK",
		},
		[]string{"stream"}, // session|track
	)

	RedisGroupEnsure = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_redis_group_ensure_total",
			Help: "ensureGroup outcomes (created/existing/error)",
		},
		[]string{"result"}, // created|exists|error
	)

	RedisGroupClaims = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_redis_group_claims_total",
			Help: "Total number of XCLAIM attempts",
		},
		[]string{"result"}, // success|empty|error
	)

	RedisGroupClaimMessages = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "relais_redis_group_claim_messages_total",
			Help: "Total number of messages successfully claimed via XCLAIM",
		},
	)

	// HTTP server metrics (core)
	HTTPInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "relais_http_requests_in_flight",
			Help: "In-flight HTTP requests",
		},
	)

	HTTPRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "relais_http_request_duration_seconds",
			Help:    "Duration of HTTP requests",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "code"},
	)

	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relais_http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "code"},
	)

	HTTPPanicsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "relais_http_panics_total",
			Help: "Total number of panics recovered in HTTP handlers",
		},
	)
)
