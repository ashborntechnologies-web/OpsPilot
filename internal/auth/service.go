package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Service validates Clerk-issued JWTs (RS256) and can fetch user details from Clerk's backend API.
type Service struct {
	jwksURL   string
	secretKey string // CLERK_SECRET_KEY — used to call Clerk Backend API

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// Claims represents the JWT claims issued by Clerk.
type Claims struct {
	jwt.RegisteredClaims
	SessionID string `json:"sid"`
	AZP       string `json:"azp"` // authorized party (frontend URL)
}

// ClerkUser is the shape of a user returned from Clerk's Backend API.
type ClerkUser struct {
	ID             string `json:"id"`
	PrimaryEmailID string `json:"primary_email_address_id"`
	EmailAddresses []struct {
		ID           string `json:"id"`
		EmailAddress string `json:"email_address"`
	} `json:"email_addresses"`
}

// NewService creates the auth service.
// publishableKey: CLERK_PUBLISHABLE_KEY (used to derive JWKS URL)
// secretKey:      CLERK_SECRET_KEY (used to call Clerk Backend API)
func NewService(publishableKey, secretKey string) *Service {
	return &Service{
		jwksURL:   deriveJWKSURL(publishableKey),
		secretKey: secretKey,
		keys:      make(map[string]*rsa.PublicKey),
	}
}

// ValidateToken validates a Clerk-issued JWT and returns the claims.
func (s *Service) ValidateToken(tokenString string) (*Claims, error) {
	// Bound JWKS fetches so a hung endpoint can't stall every authenticated request.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		kid, _ := token.Header["kid"].(string)
		return s.getPublicKey(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	return claims, nil
}

// FetchClerkUser calls Clerk's Backend API to get user details (including email).
func (s *Service) FetchClerkUser(ctx context.Context, clerkUserID string) (*ClerkUser, error) {
	url := fmt.Sprintf("https://api.clerk.com/v1/users/%s", clerkUserID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.secretKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call Clerk API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Clerk API returned %s for user %s", resp.Status, clerkUserID)
	}

	var user ClerkUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("failed to decode Clerk user: %w", err)
	}

	return &user, nil
}

// PrimaryEmail returns the user's primary email address.
func (u *ClerkUser) PrimaryEmail() string {
	for _, e := range u.EmailAddresses {
		if e.ID == u.PrimaryEmailID {
			return e.EmailAddress
		}
	}
	// Fall back to first address if primary ID doesn't match
	if len(u.EmailAddresses) > 0 {
		return u.EmailAddresses[0].EmailAddress
	}
	return ""
}

// ---- JWKS helpers ----

// deriveJWKSURL extracts the frontend API domain from a Clerk publishable key.
// Key format: pk_test_<base64url(domain)>$ or pk_live_<base64url(domain)>$
func deriveJWKSURL(publishableKey string) string {
	key := publishableKey
	for _, prefix := range []string{"pk_live_", "pk_test_"} {
		if strings.HasPrefix(key, prefix) {
			key = strings.TrimPrefix(key, prefix)
			break
		}
	}
	key = strings.TrimSuffix(key, "$")

	// Pad to valid base64 length
	switch len(key) % 4 {
	case 2:
		key += "=="
	case 3:
		key += "="
	}

	domain, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return ""
	}

	domainStr := strings.TrimSuffix(string(domain), "$")
	return fmt.Sprintf("https://%s/.well-known/jwks.json", domainStr)
}

// getPublicKey returns the RSA public key for the given kid, refreshing JWKS if stale.
func (s *Service) getPublicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	s.mu.RLock()
	key, ok := s.keys[kid]
	fresh := time.Since(s.fetchedAt) < 5*time.Minute
	s.mu.RUnlock()

	if ok && fresh {
		return key, nil
	}

	if err := s.refreshJWKS(ctx); err != nil {
		// If refresh fails but we have a cached key, use it rather than rejecting the request
		s.mu.RLock()
		key, ok = s.keys[kid]
		s.mu.RUnlock()
		if ok {
			return key, nil
		}
		return nil, err
	}

	s.mu.RLock()
	key, ok = s.keys[kid]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no JWKS key found for kid %q", kid)
	}
	return key, nil
}

// refreshJWKS fetches and parses Clerk's JWKS endpoint.
func (s *Service) refreshJWKS(ctx context.Context) error {
	if s.jwksURL == "" {
		return fmt.Errorf("JWKS URL not configured — check CLERK_PUBLISHABLE_KEY")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", s.jwksURL, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS endpoint returned %s", resp.Status)
	}

	var jwks struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("failed to decode JWKS: %w", err)
	}

	newKeys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.KTY != "RSA" {
			continue
		}
		pub, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		newKeys[k.KID] = pub
	}

	s.mu.Lock()
	s.keys = newKeys
	s.fetchedAt = time.Now()
	s.mu.Unlock()

	return nil
}

// parseRSAPublicKey builds an *rsa.PublicKey from base64url-encoded modulus and exponent.
func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode exponent: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)

	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}
