package pg

import "context"

// InstanceValue resolves a runtime-settable instance secret: the sealed
// instance_setting row wins over the env fallback; unopenable rows fall back.
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
