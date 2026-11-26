package plugin

import "testing"

// checkWebFetch allows HTTPS origin and target URLs matching permissions.network
// and rejects unpermitted hosts or insecure schemes.
func TestCheckWebFetchPermissions(t *testing.T) {
	p := &Plugin{
		manifest: Manifest{
			Permissions: Permissions{
				Network: []string{"soundcloud.com", "api-v2.soundcloud.com"},
			},
		},
	}
	if err := p.checkWebFetch(WebFetchRequest{
		Origin: "https://soundcloud.com/robots.txt",
		URL:    "https://api-v2.soundcloud.com/users/1/track_likes/2",
	}); err != nil {
		t.Fatalf("allowed hosts rejected: %v", err)
	}
	if err := p.checkWebFetch(WebFetchRequest{
		Origin: "https://evil.example/",
		URL:    "https://api-v2.soundcloud.com/users/1/track_likes/2",
	}); err == nil {
		t.Fatal("expected unpermitted origin to be rejected")
	}
	if err := p.checkWebFetch(WebFetchRequest{
		Origin: "https://soundcloud.com/robots.txt",
		URL:    "http://api-v2.soundcloud.com/users/1/track_likes/2",
	}); err == nil {
		t.Fatal("expected http URL to be rejected")
	}
}
