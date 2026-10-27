package foundrydocumentworker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	reads = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_document_reads_total",
		Help: "Document reads by format (CSV, TSV, JSON, JSONL, Parquet, PDF) and outcome: read, too_large, unsupported, retried, " +
			"released (the worker stopped), timed_out (left to lapse), lost (its claim or document went) or unrecorded.",
	}, []string{"format", "outcome"})
	readSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "helix_foundry_document_read_seconds",
		Help:    "How long one document read took, by format.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
	}, []string{"format"})
	readRows = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_document_read_rows_total",
		Help: "Rows read from table files, by format.",
	}, []string{"format"})
	readPages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "helix_foundry_document_read_pages_total",
		Help: "Pages of the PDFs read to their text.",
	})
)
