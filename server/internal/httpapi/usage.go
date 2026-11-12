package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"io.yueyang/tom/internal/domain"
	"io.yueyang/tom/internal/service"
)

func (s *Server) usageRoutes(r chi.Router) {
	r.Get("/usage", handleRead(s, func(ctx context.Context, r *http.Request) (any, error) {
		values := r.URL.Query()
		query := service.UsageQuery{GroupBy: values.Get("group_by"), Cursor: values.Get("cursor")}
		var err error
		query.Limit, err = pageLimit(r, 50)
		if err != nil {
			return nil, err
		}
		for key, target := range map[string]*time.Time{"from": &query.From, "to": &query.To} {
			if value := values.Get(key); value != "" {
				*target, err = time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return nil, domain.NewError(400, "USAGE_QUERY_INVALID", "Use RFC3339 timestamps for from and to.")
				}
			}
		}
		return s.Usage.Report(ctx, query)
	}))
}
