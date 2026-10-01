package domain

import "time"

const (
	// PrecheckTTL bounds how long a precheck answer may be treated as fresh.
	PrecheckTTL = 30 * time.Second

	// PrecheckReservationNone states that a precheck holds no nodes. It never
	// creates a lease or blocks another allocation.
	PrecheckReservationNone = "none"

	// PrecheckGuarantee states the only promise a precheck makes: formal
	// allocation re-validates every eligibility rule against a newer snapshot.
	PrecheckGuarantee = "advisory_allocation_revalidates"
)
