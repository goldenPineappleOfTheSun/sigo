package siclo

type ValidationResult struct {
	Result        bool   `json:"result"`
	Justification string `json:"justification"`
}
