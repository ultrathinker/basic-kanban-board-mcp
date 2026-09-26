package adapter

import (
	"strings"
	"testing"
)

func TestDeliveryPromptNamesPostingKeyNextToADifferentSignature(t *testing.T) {
	signed := QueuedMessage{FeedMessage: FeedMessage{ID: "m1", Author: "owner", AuthorVia: "opus-x3", Kind: "command", Body: "ship it"}}
	if got := deliveryPrompt(signed); !strings.Contains(got, "from owner (via opus-x3) (command)") {
		t.Fatalf("prompt hides the posting key: %q", got)
	}
	plain := QueuedMessage{FeedMessage: FeedMessage{ID: "m2", Author: "opus-x3", Kind: "update", Body: "done"}}
	if got := deliveryPrompt(plain); !strings.Contains(got, "from opus-x3 (update)") || strings.Contains(got, "via") {
		t.Fatalf("prompt for a self-signed message: %q", got)
	}
}
