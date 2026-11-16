package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"text/tabwriter"
	"time"
)

// writeTable prints the human-readable report.
func writeTable(w io.Writer, res Result) error {
	cfg := res.Config
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintf(tw, "workload\t%s\n", cfg.Workload)
	fmt.Fprintf(tw, "keys\t%s\n", formatInt(int64(cfg.Keys)))
	fmt.Fprintf(tw, "ops\t%s\n", formatInt(int64(cfg.Ops)))
	fmt.Fprintf(tw, "read ratio\t%s\n", formatPercent(cfg.ReadRatio*100))
	fmt.Fprintf(tw, "seed\t%d\n", cfg.Seed)
	fmt.Fprintf(tw, "concurrency\t%d\n", cfg.Concurrency)
	fmt.Fprintf(tw, "value size\t%s\n", formatBytes(int64(cfg.ValueSize)))
	if cfg.Workload == WorkloadZipfian {
		fmt.Fprintf(tw, "zipf exponent\t%g\n", cfg.ZipfS)
	}
	fmt.Fprintf(tw, "load\t%s keys in %s (excluded from throughput)\n",
		formatInt(int64(cfg.Keys)), res.LoadDuration)

	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "metric\tvalue")
	fmt.Fprintln(tw, "------\t-----")
	fmt.Fprintf(tw, "duration\t%s\n", res.Duration)
	fmt.Fprintf(tw, "throughput\t%s ops/sec\n", formatInt(int64(math.Round(res.OpsPerSec))))

	lat := res.Latency
	fmt.Fprintf(tw, "latency min\t%s\n", lat.Min)
	fmt.Fprintf(tw, "latency mean\t%s\n", lat.Mean)
	fmt.Fprintf(tw, "latency p50\t%s\n", lat.P50)
	fmt.Fprintf(tw, "latency p95\t%s\n", lat.P95)
	fmt.Fprintf(tw, "latency p99\t%s\n", lat.P99)
	fmt.Fprintf(tw, "latency p999\t%s\n", lat.P999)
	fmt.Fprintf(tw, "latency max\t%s\n", lat.Max)

	total := res.Reads + res.Writes
	fmt.Fprintf(tw, "reads\t%s (%s)\n", formatInt(res.Reads), formatPercent(percent(res.Reads, total)))
	fmt.Fprintf(tw, "writes\t%s (%s)\n", formatInt(res.Writes), formatPercent(percent(res.Writes, total)))
	fmt.Fprintf(tw, "errors\t%s\n", formatInt(res.Errors))
	if res.ErrorSample != "" {
		fmt.Fprintf(tw, "first error\t%s\n", res.ErrorSample)
	}
	fmt.Fprintf(tw, "bytes read\t%s\n", formatBytes(res.BytesRead))
	fmt.Fprintf(tw, "bytes written\t%s\n", formatBytes(res.BytesWritten))

	return tw.Flush()
}

// jsonLatency is in microseconds so the numbers stay exact to the nanosecond
// while remaining readable in a JSON report.
type jsonLatency struct {
	Min  float64 `json:"min"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	P999 float64 `json:"p999"`
	Max  float64 `json:"max"`
}

type jsonReport struct {
	Workload        string      `json:"workload"`
	Keys            int         `json:"keys"`
	Ops             int         `json:"ops"`
	ReadRatio       float64     `json:"read_ratio"`
	Seed            int64       `json:"seed"`
	Concurrency     int         `json:"concurrency"`
	ValueSize       int         `json:"value_size"`
	ZipfS           *float64    `json:"zipf_s,omitempty"`
	DurationSeconds float64     `json:"duration_seconds"`
	LoadSeconds     float64     `json:"load_seconds"`
	OpsPerSec       float64     `json:"ops_per_sec"`
	LatencyUS       jsonLatency `json:"latency_us"`
	LatencySamples  int         `json:"latency_samples"`
	Reads           int64       `json:"reads"`
	Writes          int64       `json:"writes"`
	Errors          int64       `json:"errors"`
	ErrorSample     string      `json:"error_sample,omitempty"`
	BytesRead       int64       `json:"bytes_read"`
	BytesWritten    int64       `json:"bytes_written"`
}

func buildJSONReport(res Result) jsonReport {
	cfg := res.Config
	rep := jsonReport{
		Workload:        string(cfg.Workload),
		Keys:            cfg.Keys,
		Ops:             cfg.Ops,
		ReadRatio:       cfg.ReadRatio,
		Seed:            cfg.Seed,
		Concurrency:     cfg.Concurrency,
		ValueSize:       cfg.ValueSize,
		DurationSeconds: res.Duration.Seconds(),
		LoadSeconds:     res.LoadDuration.Seconds(),
		OpsPerSec:       math.Round(res.OpsPerSec*100) / 100,
		LatencyUS: jsonLatency{
			Min:  micros(res.Latency.Min),
			Mean: micros(res.Latency.Mean),
			P50:  micros(res.Latency.P50),
			P95:  micros(res.Latency.P95),
			P99:  micros(res.Latency.P99),
			P999: micros(res.Latency.P999),
			Max:  micros(res.Latency.Max),
		},
		LatencySamples: res.Latency.Count,
		Reads:          res.Reads,
		Writes:         res.Writes,
		Errors:         res.Errors,
		ErrorSample:    res.ErrorSample,
		BytesRead:      res.BytesRead,
		BytesWritten:   res.BytesWritten,
	}
	if cfg.Workload == WorkloadZipfian {
		s := cfg.ZipfS
		rep.ZipfS = &s
	}
	return rep
}

func writeJSON(w io.Writer, res Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(buildJSONReport(res))
}

func micros(d time.Duration) float64 {
	return float64(d) / 1000
}

func percent(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}

func formatPercent(p float64) string {
	return fmt.Sprintf("%.1f%%", p)
}

func formatInt(n int64) string {
	neg := n < 0
	u := uint64(n)
	if neg {
		u = -u
	}
	s := strconv.FormatUint(u, 10)

	var b []byte
	if neg {
		b = append(b, '-')
	}
	pre := len(s) % 3
	if pre == 0 {
		pre = 3
	}
	b = append(b, s[:pre]...)
	for i := pre; i < len(s); i += 3 {
		b = append(b, ',')
		b = append(b, s[i:i+3]...)
	}
	return string(b)
}

// formatBytes uses binary units (KiB, MiB, ...).
func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(n)
	unit := ""
	for _, u := range units {
		value /= 1024
		unit = u
		if value < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}
