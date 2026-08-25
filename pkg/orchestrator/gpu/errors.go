package gpu

import "errors"

// AdmissionError is a refusal to place a GPU request, carrying the
// machine-readable reason slug alongside the operator-facing message.
//
// The numbers go in the message, not in the slug: "NoGpuCapacity" with
// "needs 20Gi; 6Gi free on the only device that matches" reads better
// than a slug that encodes its own caller, and keeps the set of slugs
// small enough to script against.
type AdmissionError struct {
	Reason  string
	Message string
}

func (e *AdmissionError) Error() string { return e.Message }

// ReasonOf returns the reason slug carried by err, or "" if err is not an
// admission refusal.
func ReasonOf(err error) string {
	var ae *AdmissionError
	if errors.As(err, &ae) {
		return ae.Reason
	}
	return ""
}
