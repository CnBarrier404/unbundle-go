package session

import "encoding/json/jsontext"

// InstructionUpdate is private recovery data, never ordinary diagnostic output.
type InstructionUpdate struct {
	Index   int            `json:"messageIndex"`
	Role    string         `json:"role"`
	Content jsontext.Value `json:"content"`
}
