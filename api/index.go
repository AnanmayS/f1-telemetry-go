// Package handler is the Vercel Function that serves /api/*. Vercel builds
// every .go file in the top-level api directory into a function; the rewrite
// in vercel.json sends all API paths here. The logic lives in vercelapp.
package handler

import (
	"net/http"

	"github.com/AnanmayS/f1-telemetry-go/vercelapp"
)

// Handler is the function entry point.
func Handler(w http.ResponseWriter, r *http.Request) { vercelapp.Handler(w, r) }
