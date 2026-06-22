// DTOs returned by the Go HTTP API (see ../../api/server.go).
//
// Times are RFC3339 strings on the wire; durations are nanoseconds as
// numbers — large enough that they exceed 2^53 only for runs longer than
// ~104 days, which is well above any plausible CI pipeline duration.

export interface Run {
	trace_id: string;
	service_name: string;
	start_time: string;
	end_time: string;
	duration_ns: number;
	span_count: number;
	status_code: string;
	command: string;
	resource_attributes: Record<string, string>;
}

export interface Span {
	span_id: string;
	parent_span_id: string;
	name: string;
	kind: string;
	service_name: string;
	start_time: string;
	duration_ns: number;
	status_code: string;
	status_message: string;
	span_attributes: Record<string, string>;
}

export interface Trace {
	trace_id: string;
	spans: Span[];
}

export interface LogRecord {
	timestamp: string;
	trace_id: string;
	span_id: string;
	severity_number: number;
	severity_text: string;
	service_name: string;
	body: string;
	log_attributes: Record<string, string>;
}
