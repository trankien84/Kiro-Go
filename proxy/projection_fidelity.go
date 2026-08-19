package proxy

// requestProjectionFidelity is the shared request-side semantic fidelity
// primitive used by compatibility projectors. It deliberately says nothing
// about candidate ordering, health, retries, or authentication lifecycle.
type requestProjectionFidelity uint8

const (
	requestProjectionExact requestProjectionFidelity = iota
	requestProjectionEmulated
	requestProjectionUnsupported
)

func (fidelity requestProjectionFidelity) String() string {
	switch fidelity {
	case requestProjectionExact:
		return "exact"
	case requestProjectionEmulated:
		return "emulated"
	case requestProjectionUnsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}
