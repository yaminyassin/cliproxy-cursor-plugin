package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// jwtClaims is the minimal JWT payload shape needed to read the standard
// "exp" claim, ported from gajae-code's getTokenExpiry.
type jwtClaims struct {
	Exp float64 `json:"exp"`
}

// defaultExpiryFallback is used when a token cannot be parsed as a JWT,
// matching gajae-code's own fallback (now + 1 hour).
const defaultExpiryFallback = time.Hour

// refreshSkew is subtracted from the parsed expiry so the host refreshes
// slightly before the token actually expires, matching gajae-code's
// 5-minute skew (decoded.exp * 1000 - 5 * 60 * 1000).
const refreshSkew = 5 * time.Minute

// getTokenExpiry parses a Cursor access token as a JWT and returns its
// expiry time minus a 5-minute refresh skew, or now+1h if the token is
// not a well-formed JWT with a numeric "exp" claim. Ported from
// gajae-code's getTokenExpiry in packages/ai/src/utils/oauth/cursor.ts.
func getTokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return time.Now().Add(defaultExpiryFallback)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Now().Add(defaultExpiryFallback)
	}

	var claims jwtClaims
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil || claims.Exp == 0 {
		return time.Now().Add(defaultExpiryFallback)
	}

	expiry := time.Unix(int64(claims.Exp), 0)
	return expiry.Add(-refreshSkew)
}
