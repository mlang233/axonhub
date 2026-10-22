package provider_quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/llm/httpclient"
)

const ollamaBalanceURL = "https://ollama.com/api/balance"

// OllamaBalancePeriod is the included-credit period returned by the official
// balance API.
type OllamaBalancePeriod struct {
	From  string `json:"from,omitempty"`
	Until string `json:"until,omitempty"`
}

// OllamaLegacyBalanceLimit is the session/weekly limit shape returned for
// legacy plans.
type OllamaLegacyBalanceLimit struct {
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
	ResetsAt         string   `json:"resets_at,omitempty"`
}

// OllamaIncludedBalance is the "included" object returned by GET /api/balance.
// New credit plans use balance_usd/allowance_usd/period; legacy plans use
// session/weekly instead.
type OllamaIncludedBalance struct {
	BalanceUSD   *float64                  `json:"balance_usd,omitempty"`
	AllowanceUSD *float64                  `json:"allowance_usd,omitempty"`
	Period       *OllamaBalancePeriod      `json:"period,omitempty"`
	Session      *OllamaLegacyBalanceLimit `json:"session,omitempty"`
	Weekly       *OllamaLegacyBalanceLimit `json:"weekly,omitempty"`
}

// OllamaPurchasedBalance is the "purchased" object returned by GET
// /api/balance.
type OllamaPurchasedBalance struct {
	BalanceUSD *float64 `json:"balance_usd,omitempty"`
}

// OllamaBalanceResponse is the payload of GET https://ollama.com/api/balance.
type OllamaBalanceResponse struct {
	Included  *OllamaIncludedBalance  `json:"included"`
	Purchased *OllamaPurchasedBalance `json:"purchased"`
}

func (c *OllamaQuotaChecker) checkQuotaWithAPIKey(ctx context.Context, ch *ent.Channel, apiKey string) (QuotaData, error) {
	hc := c.httpClient
	if ch.Settings != nil && ch.Settings.Proxy != nil {
		hc = c.httpClient.WithProxy(ch.Settings.Proxy)
	}
	hc = hc.WithRejectHTTPSDowngrade()

	request := httpclient.NewRequestBuilder().
		WithMethod(http.MethodGet).
		WithURL(ollamaBalanceURL).
		WithBearerToken(apiKey).
		WithHeader("Accept", "application/json").
		Build()

	resp, err := hc.Do(ctx, request)
	if err != nil {
		if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
			if httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden {
				return QuotaData{}, fmt.Errorf("%w: Ollama balance API returned %d", ErrInvalidCredentials, httpErr.StatusCode)
			}
			return QuotaData{}, fmt.Errorf("Ollama balance API returned %d", httpErr.StatusCode)
		}
		return QuotaData{}, fmt.Errorf("Ollama balance request failed: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return QuotaData{}, fmt.Errorf("Ollama balance API returned %d", resp.StatusCode)
	}

	return c.parseBalanceResponse(resp.Body)
}

func (c *OllamaQuotaChecker) parseBalanceResponse(body []byte) (QuotaData, error) {
	var resp OllamaBalanceResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return QuotaData{}, fmt.Errorf("failed to parse Ollama balance response: %w", err)
	}
	if resp.Included == nil && resp.Purchased == nil {
		return QuotaData{}, errors.New("Ollama balance response contains neither included nor purchased balance")
	}

	raw := map[string]any{}
	limits := make([]QuotaLimitStatus, 0, 3)
	rawWindows := map[string]any{}

	var includedBalance float64
	includedOK := false
	var periodStart, periodUntil *time.Time

	if resp.Included != nil {
		raw["included"] = resp.Included

		var err error
		includedBalance, includedOK, err = ollamaBalanceAmount(resp.Included.BalanceUSD, "included.balance_usd")
		if err != nil {
			return QuotaData{}, err
		}

		if includedOK {
			allowance, allowanceOK, err := ollamaBalanceAmount(resp.Included.AllowanceUSD, "included.allowance_usd")
			if err != nil {
				return QuotaData{}, err
			}
			if !allowanceOK {
				return QuotaData{}, errors.New("Ollama balance response is missing included.allowance_usd")
			}

			periodStart, err = ollamaBalanceTime(resp.Included.Period, "period.from", true)
			if err != nil {
				return QuotaData{}, err
			}
			periodUntil, err = ollamaBalanceTime(resp.Included.Period, "period.until", false)
			if err != nil {
				return QuotaData{}, err
			}

			// A zero allowance is a valid free-plan shape; treat any remaining
			// balance as usable and otherwise mark the included window exhausted.
			usageRatio := 1.0
			if allowance > 0 {
				usageRatio = 1 - includedBalance/allowance
			} else if includedBalance > 0 {
				usageRatio = 0
			}
			if usageRatio < 0 {
				usageRatio = 0
			}
			if usageRatio > 1 {
				usageRatio = 1
			}
			status := normalizeOllamaWindowStatus(usageRatio)
			limit := NewTokenLimitStatus(status, usageRatio, periodUntil)
			limit.Window = QuotaWindowMonthly
			limit.PeriodStart = periodStart
			limits = append(limits, limit)
		}

		if err := addOllamaLegacyLimit(&limits, rawWindows, resp.Included.Session, QuotaWindow5h, 5*time.Hour); err != nil {
			return QuotaData{}, err
		}
		if err := addOllamaLegacyLimit(&limits, rawWindows, resp.Included.Weekly, QuotaWindowWeekly, 7*24*time.Hour); err != nil {
			return QuotaData{}, err
		}
	}

	purchasedBalance := 0.0
	purchasedOK := false
	if resp.Purchased != nil {
		raw["purchased"] = resp.Purchased

		var err error
		purchasedBalance, purchasedOK, err = ollamaBalanceAmount(resp.Purchased.BalanceUSD, "purchased.balance_usd")
		if err != nil {
			return QuotaData{}, err
		}
		if purchasedOK && purchasedBalance > 0 {
			limits = append(limits, NewTokenLimitStatus("available", 0, nil).WithWindow(QuotaWindowCredits, 0))
		}
	}

	if len(limits) == 0 {
		return QuotaData{}, errors.New("Ollama balance response contains no usable quota limits")
	}
	if len(rawWindows) > 0 {
		raw["windows"] = rawWindows
	}

	// Keep the table's monetary view aligned with the API for credit plans.
	// Legacy plans keep their session/weekly percentage rows; their purchased
	// balance is shown in the quota popover instead of replacing those rows.
	if includedOK || (len(rawWindows) == 0 && purchasedOK && purchasedBalance > 0) {
		total := includedBalance
		if purchasedOK {
			total += purchasedBalance
		}
		balance := map[string]any{
			"currency":      "USD",
			"total_balance": total,
		}
		if includedOK && purchasedOK {
			balance["topped_up_balance"] = purchasedBalance
		}
		raw["balances"] = []map[string]any{balance}
	}

	return NormalizeQuotaData(QuotaData{
		Status:       "unknown",
		ProviderType: ollamaProviderType,
		RawData:      raw,
		Limits:       limits,
	}), nil
}

func addOllamaLegacyLimit(
	limits *[]QuotaLimitStatus,
	rawWindows map[string]any,
	window *OllamaLegacyBalanceLimit,
	windowKey string,
	duration time.Duration,
) error {
	if window == nil {
		return nil
	}
	if window.RemainingPercent == nil {
		return fmt.Errorf("Ollama balance response is missing %s.remaining_percent", windowKey)
	}

	remaining := *window.RemainingPercent
	if math.IsNaN(remaining) || math.IsInf(remaining, 0) {
		return fmt.Errorf("Ollama balance response has an invalid %s.remaining_percent", windowKey)
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 100 {
		remaining = 100
	}

	resetAt, err := ollamaBalanceTimeValue(window.ResetsAt, windowKey+".resets_at")
	if err != nil {
		return err
	}

	usageRatio := (100 - remaining) / 100
	status := normalizeOllamaWindowStatus(usageRatio)
	limit := NewTokenLimitStatus(status, usageRatio, resetAt).WithWindow(windowKey, duration)
	*limits = append(*limits, limit)

	rawWindow := map[string]any{
		"usage_percent":     usageRatio * 100,
		"status":            status,
		"percent_remaining": remaining,
	}
	if resetAt != nil {
		rawWindow["reset_time"] = resetAt.Format(time.RFC3339)
	}
	rawWindows[windowKey] = rawWindow

	return nil
}

func ollamaBalanceAmount(value *float64, field string) (float64, bool, error) {
	if value == nil {
		return 0, false, nil
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		return 0, false, fmt.Errorf("Ollama balance response has an invalid %s", field)
	}
	return *value, true, nil
}

func ollamaBalanceTime(period *OllamaBalancePeriod, field string, from bool) (*time.Time, error) {
	if period == nil {
		return nil, nil
	}
	value := period.Until
	if from {
		value = period.From
	}
	return ollamaBalanceTimeValue(value, field)
}

func ollamaBalanceTimeValue(value, field string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("Ollama balance response has an invalid %s: %w", field, err)
	}
	return &parsed, nil
}

func ollamaAPIKey(ch *ent.Channel) string {
	for _, apiKey := range ch.Credentials.GetAllAPIKeys() {
		if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
			return apiKey
		}
	}
	return ""
}

func ollamaHasCookie(ch *ent.Channel) bool {
	return ch.Settings != nil &&
		ch.Settings.ProviderQuota != nil &&
		ch.Settings.ProviderQuota.Ollama != nil &&
		strings.TrimSpace(ch.Settings.ProviderQuota.Ollama.AuthCookie) != ""
}

// HasOllamaQuotaCredentials reports whether an Ollama channel can collect
// quota with either its inference API key or a legacy browser session cookie.
func HasOllamaQuotaCredentials(ch *ent.Channel) bool {
	if ch.Type != channel.TypeOllama && ch.Type != channel.TypeOllamaAnthropic {
		return false
	}
	if ollamaAPIKey(ch) != "" {
		return true
	}
	if !ollamaHasCookie(ch) {
		return false
	}
	_, err := NormalizeOllamaCookie(ch.Settings.ProviderQuota.Ollama.AuthCookie)
	return err == nil
}
