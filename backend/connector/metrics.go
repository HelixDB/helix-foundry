package connector

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	s3AssumeRole = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "foundry_connector_s3_assume_role_total",
		Help: "Assumptions of customers' S3 roles by outcome: ok, denied, unenforced (the trust lacks the external ID) or error.",
	}, []string{"outcome"})
	s3Files = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "foundry_connector_s3_files_total",
		Help: "S3 files a sync read, by format and outcome: ok, malformed, denied or changed.",
	}, []string{"format", "outcome"})
	s3Bytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "foundry_connector_s3_bytes_total",
		Help: "Bytes syncs downloaded from customers' buckets, by format.",
	}, []string{"format"})
	s3ValuesNulled = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "foundry_connector_s3_values_nulled_total",
		Help: "Values written as null because JSON cannot hold them, by reason: timestamp_range or non_finite.",
	}, []string{"reason"})
	cdcResets = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_cdc_resets_total",
		Help: "Reads in full a database's log forced, by kind and reason: missing, lost or purged for every table; schema, columns, key, volume or truncate for one.",
	}, []string{"kind", "reason"})
	cdcRetainedBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "helix_foundry_cdc_retained_bytes",
		Help: "WAL a PostgreSQL connector's replication slot held on the customer's database at its last change read.",
	}, []string{"connector"})
)
