package api

import (
	"testing"
	"time"
)

func TestConfigApplyDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "empty config gets default table names and dial timeout",
			in:   Config{},
			want: Config{
				TracesTable: "otel_traces",
				LogsTable:   "otel_logs",
				DialTimeout: 30 * time.Second,
			},
		},
		{
			name: "explicit table names are preserved",
			in: Config{
				TracesTable: "custom_traces",
				LogsTable:   "custom_logs",
			},
			want: Config{
				TracesTable: "custom_traces",
				LogsTable:   "custom_logs",
				DialTimeout: 30 * time.Second,
			},
		},
		{
			name: "explicit dial timeout survives defaulting",
			in:   Config{DialTimeout: 5 * time.Second},
			want: Config{
				TracesTable: "otel_traces",
				LogsTable:   "otel_logs",
				DialTimeout: 5 * time.Second,
			},
		},
		{
			name: "connection options are not invented when zero",
			in:   Config{},
			want: Config{
				TracesTable: "otel_traces",
				LogsTable:   "otel_logs",
				DialTimeout: 30 * time.Second,
			},
		},
		{
			name: "explicit connection options survive defaulting",
			in: Config{
				ReadTimeout:  2 * time.Second,
				MaxOpenConns: 16,
				MaxIdleConns: 8,
				Compression:  true,
			},
			want: Config{
				TracesTable:  "otel_traces",
				LogsTable:    "otel_logs",
				DialTimeout:  30 * time.Second,
				ReadTimeout:  2 * time.Second,
				MaxOpenConns: 16,
				MaxIdleConns: 8,
				Compression:  true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in
			got.applyDefaults()
			if got != tt.want {
				t.Errorf("applyDefaults = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHexIDPattern(t *testing.T) {
	tests := []struct {
		in    string
		match bool
	}{
		{"", false},
		{"abcdef0123456789", true},                   // 16 chars (span id)
		{"abcdef0123456789abcdef0123456789", true},   // 32 chars (trace id)
		{"abcdef0123456789abcdef0123456789a", false}, // 33 chars
		{"ABCDEF0123456789", true},                   // upper case allowed
		{"not-hex!", false},                          // punctuation
		{"abcdef0123456789abcdef0123456789DROP TABLE otel_traces", false}, // injection attempt
	}
	for _, tt := range tests {
		got := hexIDPattern.MatchString(tt.in)
		if got != tt.match {
			t.Errorf("MatchString(%q) = %v, want %v", tt.in, got, tt.match)
		}
	}
}
