package responses

import (
	"encoding/json"
)

type sseEvent struct {
	Type         string       `json:"type"`
	Delta        string       `json:"delta"`
	Text         string       `json:"text"`
	Arguments    string       `json:"arguments"`
	ItemID       string       `json:"item_id"`
	ContentIndex int          `json:"content_index"`
	Item         *sseItem     `json:"item"`
	Response     *sseResponse `json:"response"`
}

type sseItem struct {
	ID, Type, CallID, Name, Arguments, Status string
	Raw                                       json.RawMessage
}

func (i *sseItem) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*i = sseItem{ID: wire.ID, Type: wire.Type, CallID: wire.CallID, Name: wire.Name, Arguments: wire.Arguments, Status: wire.Status, Raw: append(json.RawMessage(nil), data...)}
	return nil
}

type sseResponse struct {
	ID                string            `json:"id"`
	Usage             *sseUsage         `json:"usage"`
	Error             *sseError         `json:"error"`
	IncompleteDetails incompleteDetails `json:"incomplete_details"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}
type sseError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}
type sseUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}
