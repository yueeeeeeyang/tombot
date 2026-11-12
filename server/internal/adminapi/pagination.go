package adminapi

import (
	"net/http"
	"net/url"
	"strconv"
)

func (handler *Handler) listPage(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		handler.failure(w, r, 400, "INPUT_INVALID", "分页参数无效", nil)
		return "", 0, false
	}
	for key, values := range query {
		if (key != "before" && key != "limit") || len(values) != 1 {
			handler.failure(w, r, 400, "INPUT_INVALID", "分页参数无效", nil)
			return "", 0, false
		}
	}
	limit := 50
	if value, exists := query["limit"]; exists {
		limit, err = strconv.Atoi(value[0])
		if err != nil || limit < 1 || limit > 100 {
			handler.failure(w, r, 400, "INPUT_INVALID", "分页参数无效", nil)
			return "", 0, false
		}
	}
	if len(query.Get("before")) > 26 {
		handler.failure(w, r, 400, "INPUT_INVALID", "分页参数无效", nil)
		return "", 0, false
	}
	return query.Get("before"), limit, true
}
