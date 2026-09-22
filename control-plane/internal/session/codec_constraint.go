package session

import (
	"errors"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/profile"
)

// constrainedCodec is the codec a placement refusal was constrained to, or ""
// when the launch carried no codec constraint.
func constrainedCodec(err error) string {
	var cr *CodecConstraintRejection
	if errors.As(err, &cr) {
		return cr.Codec
	}
	return ""
}

// codecConstraint returns the launch's codec constraint (CreateParams.RequireCodec):
// the explicit stream.codec, or "" for an Auto launch.
//
// A codec no rung of the chain uses is refused here, before placement, with
// the same ErrRungCodecNotAvailable (400) clamp 0 returns. Left to clamp 0, the
// placement gate would first refuse it as a 503 on any fleet with no GPU that
// encodes it. The legacy/tier path has no chain, so only the gate applies.
func codecConstraint(chain profile.LaunchProfile, ov StreamOverride) (string, error) {
	codec := ov.codecOverride()
	if codec == "" || chain.ID == "" {
		return codec, nil
	}
	for _, r := range chain.Rungs {
		if wire, ok := catalogToWire(r.Codec); ok && wire == codec {
			return codec, nil
		}
	}
	return "", fmt.Errorf("%w: %s (launch profile %q)", ErrRungCodecNotAvailable, codec, chain.ID)
}
