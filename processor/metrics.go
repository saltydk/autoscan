package processor

// MetricsSnapshot contains process-local counters. Each field is read atomically;
// concurrent activity may occur between field reads.
type MetricsSnapshot struct {
	// Received counts input scan events after successful persistence, before coalescing.
	Received int64
	// Processed counts successfully acknowledged deliveries. Legacy shared dispatch
	// counts one completed folder when at least one target accepts it.
	Processed int64
	// Retried counts retryable target failures that schedule a retry, including
	// availability checks and failed deliveries.
	Retried int64
	// Skipped counts acknowledged deliveries outside the target's libraries.
	Skipped int64
	// Rejected counts acknowledged deliveries deliberately refused by a target.
	Rejected int64
}

// Metrics returns cumulative counters for this processor instance.
func (p *Processor) Metrics() MetricsSnapshot {
	return MetricsSnapshot{
		Received:  p.received.Load(),
		Processed: p.processed.Load(),
		Retried:   p.retried.Load(),
		Skipped:   p.skipped.Load(),
		Rejected:  p.rejected.Load(),
	}
}

// RecordRetry records a retryable target failure when its worker schedules retry.
func (p *Processor) RecordRetry() {
	p.retried.Add(1)
}

func (p *Processor) recordReceived(count int) {
	p.received.Add(int64(count))
}

func (p *Processor) recordProcessed() {
	p.processed.Add(1)
}

func (p *Processor) recordSkipped() {
	p.skipped.Add(1)
}

func (p *Processor) recordRejected() {
	p.rejected.Add(1)
}
