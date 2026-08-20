package pg

import "context"

// InstanceValue resolves a runtime-settable instance secret: the sealed
// instance_setting row (written by the self-host Settings screen) wins over
// the env-provided fallback, so a value pasted into the UI takes effect
// without touching files. open decrypts what config.SecretKey.Seal produced;
// nil (no UC_SECRET_KEY_HEX) skips the table entirely. A row that fails to
// open falls back rather than erroring: a re-keyed instance degrades to its
// env config instead of losing the feature.
func (p *Pool) InstanceValue(ctx context.Context, open func([]byte) ([]byte, error), key, fallback string) string {
	if open != nil {
		if enc, err := p.Queries().GetInstanceSetting(ctx, key); err == nil {
			if v, oerr := open(enc); oerr == nil && len(v) > 0 {
				return string(v)
			}
		}
	}
	return fallback
}
