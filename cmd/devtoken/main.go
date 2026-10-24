// Command devtoken mints an HS256 JWT for local testing against the demo
// config. It reads the secret from JWT_SECRET (same variable the gateway uses).
//
//	go run ./cmd/devtoken -sub customer-42 -role customer -ttl 1h
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/AkibUllah/go-api-gateway/internal/auth"
)

func main() {
	sub := flag.String("sub", "customer-42", "subject claim")
	role := flag.String("role", "customer", "role claim")
	iss := flag.String("iss", "https://auth.dokan.local", "issuer claim")
	aud := flag.String("aud", "dokan-api", "audience claim")
	ttl := flag.Duration("ttl", time.Hour, "token lifetime")
	flag.Parse()

	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "dev-only-secret-change-me-0123456789abcdef" //nolint:gosec // matches the documented local-demo default in configs/gateway.yaml
	}
	tok, err := auth.SignHS256(secret, jwt.MapClaims{
		"sub": *sub, "role": *role, "iss": *iss, "aud": *aud,
	}, *ttl)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
