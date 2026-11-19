package computeragent

import (
	"context"
	"errors"
)

type browserResult struct {
	Observation *BrowserObservation `json:"observation,omitempty"`
	Error       string              `json:"error,omitempty"`
}

func (client *Client) CloseBrowser(ctx context.Context, grant BrowserGrant) (BrowserObservation, error) {
	if err := grant.Validate(); err != nil {
		return BrowserObservation{}, err
	}
	var result browserResult
	if err := client.exchange(ctx, "browser_close", grant, nil, &result); err != nil {
		return BrowserObservation{}, err
	}
	if result.Error != "" || result.Observation == nil || result.Observation.Grant != grant || result.Observation.Phase != "closed" {
		return BrowserObservation{}, errors.New("旧浏览器关闭回执未确认")
	}
	return *result.Observation, nil
}
