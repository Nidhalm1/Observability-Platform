package main

import "testing"

func TestVerify(t *testing.T) {
	tests := []struct {
		name  string
		order OrderRequest
		want  bool
	}{
		{
			name:  "valid",
			order: OrderRequest{CustomerID: 1, Items: []Item{{SKU: "ABC", Qty: 2}}},
			want:  true,
		},
		{
			name:  "missing customer",
			order: OrderRequest{Items: []Item{{SKU: "ABC", Qty: 2}}},
			want:  false,
		},
		{
			name:  "negative customer",
			order: OrderRequest{CustomerID: -1, Items: []Item{{SKU: "ABC", Qty: 2}}},
			want:  false,
		},
		{
			name:  "no items",
			order: OrderRequest{CustomerID: 1, Items: []Item{}},
			want:  false,
		},
		{
			name:  "empty sku",
			order: OrderRequest{CustomerID: 1, Items: []Item{{SKU: "", Qty: 2}}},
			want:  false,
		},
		{
			name:  "zero qty",
			order: OrderRequest{CustomerID: 1, Items: []Item{{SKU: "ABC", Qty: 0}}},
			want:  false,
		},
		{
			// One bad item invalidates the order -- the loop must not stop at
			// the first valid one.
			name: "second item invalid",
			order: OrderRequest{CustomerID: 1, Items: []Item{
				{SKU: "ABC", Qty: 2},
				{SKU: "DEF", Qty: -3},
			}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verify(tt.order); got != tt.want {
				t.Errorf("verify() = %v, want %v", got, tt.want)
			}
		})
	}
}
