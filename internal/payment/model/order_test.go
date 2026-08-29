package model

import "testing"

func TestOutStatus(t *testing.T) {
	tests := []struct {
		name     string
		input    int32
		expected int32
	}{
		{
			name:     "created status maps to 1",
			input:    OrderStatusCreated,
			expected: 1,
		},
		{
			name:     "sent status maps to 1",
			input:    OrderStatusSent,
			expected: 1,
		},
		{
			name:     "success status maps to 2",
			input:    OrderStatusSuccess,
			expected: 2,
		},
		{
			name:     "failed status maps to 3",
			input:    OrderStatusFailed,
			expected: 3,
		},
		{
			name:     "unknown status maps to 0",
			input:    999,
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := OutStatus(tt.input)
			if result != tt.expected {
				t.Errorf("OutStatus(%d) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}
