package provider_quota

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func ollamaAPIKeyChannel(apiKey string) *ent.Channel {
	return &ent.Channel{
		Type: channel.TypeOllama,
		Credentials: objects.ChannelCredentials{
			APIKey: apiKey,
		},
	}
}

func TestOllamaBalance_CheckQuota_CreditPlan(t *testing.T) {
	httpClient := httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			require.Equal(t, http.MethodGet, req.Method)
			require.Equal(t, ollamaBalanceURL, req.URL.String())
			require.Equal(t, "Bearer test-api-key", req.Header.Get("Authorization"))
			require.Equal(t, "application/json", req.Header.Get("Accept"))

			body := `{
				"included": {
					"balance_usd": 72.5,
					"allowance_usd": 100,
					"period": {"from":"2099-01-01T00:00:00Z","until":"2099-02-01T00:00:00Z"}
				},
				"purchased": {"balance_usd": 25}
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	})

	checker := NewOllamaQuotaChecker(httpClient)
	quota, err := checker.CheckQuota(context.Background(), ollamaAPIKeyChannel("test-api-key"))
	require.NoError(t, err)
	require.Equal(t, "available", quota.Status)
	require.True(t, quota.Ready)
	require.Equal(t, ollamaProviderType, quota.ProviderType)
	require.Len(t, quota.Limits, 2)

	monthly := findOllamaLimit(t, quota.Limits, QuotaWindowMonthly)
	require.Equal(t, "available", monthly.Status)
	require.InDelta(t, 0.275, monthly.UsageRatio, 0.001)
	require.NotNil(t, monthly.NextResetAt)
	require.NotNil(t, monthly.PeriodStart)

	credits := findOllamaLimit(t, quota.Limits, QuotaWindowCredits)
	require.Equal(t, "available", credits.Status)
	require.Equal(t, 0.0, credits.UsageRatio)

	balances, ok := quota.RawData["balances"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, balances, 1)
	require.Equal(t, "USD", balances[0]["currency"])
	require.InDelta(t, 97.5, balances[0]["total_balance"], 0.001)
	require.InDelta(t, 25.0, balances[0]["topped_up_balance"], 0.001)
}

func TestOllamaBalance_CheckQuota_ExhaustedAllowanceFallsBackToPurchasedCredits(t *testing.T) {
	httpClient := httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{
				"included": {
					"balance_usd": 0,
					"allowance_usd": 100,
					"period": {"from":"2099-01-01T00:00:00Z","until":"2099-02-01T00:00:00Z"}
				},
				"purchased": {"balance_usd": 25}
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	})

	checker := NewOllamaQuotaChecker(httpClient)
	quota, err := checker.CheckQuota(context.Background(), ollamaAPIKeyChannel("test-api-key"))
	require.NoError(t, err)
	require.Equal(t, "available", quota.Status)
	require.Len(t, quota.Limits, 2)
	require.Equal(t, "exhausted", findOllamaLimit(t, quota.Limits, QuotaWindowMonthly).Status)

	routing, reason := EvaluateQuotaRouting(quota.Limits, quota.Status, QuotaLimitTypeToken, time.Date(2099, 1, 15, 0, 0, 0, 0, time.UTC))
	require.Equal(t, RoutingStickyOnly, routing)
	require.Equal(t, "window_exhausted_balance_fallback", reason)
}

func TestOllamaBalance_CheckQuota_ExhaustedAllowanceWithoutCredits(t *testing.T) {
	checker := NewOllamaQuotaChecker(nil)
	quota, err := checker.parseBalanceResponse([]byte(`{
		"included": {
			"balance_usd": 0,
			"allowance_usd": 100,
			"period": {"from":"2099-01-01T00:00:00Z","until":"2099-02-01T00:00:00Z"}
		},
		"purchased": {"balance_usd": 0}
	}`))
	require.NoError(t, err)
	require.Equal(t, "exhausted", quota.Status)
	require.False(t, quota.Ready)
	require.Len(t, quota.Limits, 1)
	require.Equal(t, "exhausted", quota.Limits[0].Status)
}

func TestOllamaBalance_CheckQuota_LegacyPlan(t *testing.T) {
	checker := NewOllamaQuotaChecker(nil)
	quota, err := checker.parseBalanceResponse([]byte(`{
		"included": {
			"session": {"remaining_percent": 75, "resets_at": "2099-01-01T05:00:00Z"},
			"weekly": {"remaining_percent": 40, "resets_at": "2099-01-08T00:00:00Z"}
		},
		"purchased": {"balance_usd": 25}
	}`))
	require.NoError(t, err)
	require.Equal(t, "available", quota.Status)
	require.Len(t, quota.Limits, 3)

	session := findOllamaLimit(t, quota.Limits, QuotaWindow5h)
	require.Equal(t, "available", session.Status)
	require.InDelta(t, 0.25, session.UsageRatio, 0.001)
	weekly := findOllamaLimit(t, quota.Limits, QuotaWindowWeekly)
	require.Equal(t, "available", weekly.Status)
	require.InDelta(t, 0.6, weekly.UsageRatio, 0.001)
	credits := findOllamaLimit(t, quota.Limits, QuotaWindowCredits)
	require.Equal(t, "available", credits.Status)

	windows, ok := quota.RawData["windows"].(map[string]any)
	require.True(t, ok)
	require.Len(t, windows, 2)
	require.NotContains(t, quota.RawData, "balances")
}

func TestOllamaBalance_CheckQuota_LegacyPlanWithoutPurchasedCreditsHasNoMonetaryRow(t *testing.T) {
	checker := NewOllamaQuotaChecker(nil)
	quota, err := checker.parseBalanceResponse([]byte(`{
		"included": {
			"session": {"remaining_percent": 75, "resets_at": "2099-01-01T05:00:00Z"},
			"weekly": {"remaining_percent": 40, "resets_at": "2099-01-08T00:00:00Z"}
		},
		"purchased": {"balance_usd": 0}
	}`))
	require.NoError(t, err)
	require.Len(t, quota.Limits, 2)
	require.NotContains(t, quota.RawData, "balances")
}

func TestOllamaBalance_CheckQuota_APIKeyRejectedFallsBackToCookie(t *testing.T) {
	settingsCalls := 0
	httpClient := httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.String() {
			case ollamaBalanceURL:
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid key"}`)),
				}, nil
			case ollamaSettingsURL:
				settingsCalls++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(ollamaSettingsHTML())),
				}, nil
			default:
				t.Fatalf("unexpected URL %s", req.URL.String())
				return nil, nil
			}
		}),
	})

	checker := NewOllamaQuotaChecker(httpClient)
	ch := ollamaChannel("__Secure-session=aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789")
	ch.Credentials.APIKey = "test-api-key"

	quota, err := checker.CheckQuota(context.Background(), ch)
	require.NoError(t, err)
	require.Equal(t, 1, settingsCalls)
	require.Len(t, quota.Limits, 2)
	require.Equal(t, QuotaWindow5h, quota.Limits[0].Window)
}

func TestOllamaBalance_CheckQuota_ServerErrorDoesNotFallBackToCookie(t *testing.T) {
	settingsCalls := 0
	httpClient := httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() == ollamaSettingsURL {
				settingsCalls++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(ollamaSettingsHTML())),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":"unavailable"}`)),
			}, nil
		}),
	})

	checker := NewOllamaQuotaChecker(httpClient)
	ch := ollamaChannel("__Secure-session=aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789")
	ch.Credentials.APIKey = "test-api-key"

	_, err := checker.CheckQuota(context.Background(), ch)
	require.Error(t, err)
	require.Equal(t, 0, settingsCalls)
}

func TestOllamaBalance_CheckQuota_APIKeyRejectedWithoutCookie(t *testing.T) {
	httpClient := httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":"forbidden"}`)),
			}, nil
		}),
	})

	checker := NewOllamaQuotaChecker(httpClient)
	_, err := checker.CheckQuota(context.Background(), ollamaAPIKeyChannel("bad-key"))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestOllamaBalance_ParseResponse_Errors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "malformed json", body: `{invalid json}`, wantErr: "failed to parse"},
		{name: "missing included and purchased", body: `{}`, wantErr: "neither included nor purchased"},
		{name: "missing allowance", body: `{"included":{"balance_usd":10}}`, wantErr: "missing included.allowance_usd"},
		{name: "negative included balance", body: `{"included":{"balance_usd":-1,"allowance_usd":100}}`, wantErr: "invalid included.balance_usd"},
		{name: "missing legacy remaining percent", body: `{"included":{"session":{}}}`, wantErr: "missing 5h.remaining_percent"},
		{name: "invalid legacy reset", body: `{"included":{"session":{"remaining_percent":50,"resets_at":"bad"}}}`, wantErr: "invalid 5h.resets_at"},
		{name: "no usable limits", body: `{"included":{},"purchased":{"balance_usd":0}}`, wantErr: "no usable quota limits"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := NewOllamaQuotaChecker(nil)
			_, err := checker.parseBalanceResponse([]byte(tt.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestOllamaBalance_ParseResponse_ZeroAllowance(t *testing.T) {
	checker := NewOllamaQuotaChecker(nil)
	quota, err := checker.parseBalanceResponse([]byte(`{
		"included": {"balance_usd":0,"allowance_usd":0},
		"purchased": {"balance_usd":0}
	}`))
	require.NoError(t, err)
	require.Equal(t, "exhausted", quota.Status)
	require.Len(t, quota.Limits, 1)
	require.Equal(t, "exhausted", quota.Limits[0].Status)
	require.Equal(t, 1.0, quota.Limits[0].UsageRatio)
}

func TestOllamaBalance_SupportsChannelWithAPIKey(t *testing.T) {
	checker := NewOllamaQuotaChecker(nil)
	require.True(t, checker.SupportsChannel(ollamaAPIKeyChannel("test-api-key")))
	require.True(t, checker.SupportsChannel(ollamaChannel("__Secure-session=aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789")))
	require.False(t, checker.SupportsChannel(ollamaAPIKeyChannel("")))
	require.False(t, checker.SupportsChannel(&ent.Channel{Type: channel.TypeOpenai, Credentials: objects.ChannelCredentials{APIKey: "test-api-key"}}))
}

func findOllamaLimit(t *testing.T, limits []QuotaLimitStatus, window string) QuotaLimitStatus {
	t.Helper()
	for _, limit := range limits {
		if limit.Window == window {
			return limit
		}
	}
	t.Fatalf("limit %s not found in %+v", window, limits)
	return QuotaLimitStatus{}
}
