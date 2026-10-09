package relay

import (
	"fmt"

	"github.com/zzstar101/mytoken/internal/source"
)

// newClient builds the adapter for a detected site. The site's Kind decides the
// shape; a zero Kind is treated as unknown, which only supports manual ratios.
func newClient(h *HTTP, site Site, cred source.Credential) (Client, error) {
	switch site.Kind {
	case KindNewAPI:
		return &newAPIClient{h: h, site: site, cred: cred}, nil
	case KindSub2API:
		return &sub2APIClient{h: h, site: site, cred: cred}, nil
	case KindOpenRouter, KindDeepSeek, KindSilicon, KindMoonshot, KindZAI, KindMiniMax:
		return &officialClient{h: h, site: site, cred: cred, kind: site.Kind}, nil
	case KindUnknown, "":
		return nil, fmt.Errorf("relay: site %s has unknown kind; set ratios manually", site.Origin)
	}
	return nil, fmt.Errorf("relay: unsupported kind %q", site.Kind)
}
