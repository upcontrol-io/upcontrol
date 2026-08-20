/**
 * "upcontrol.io", "https://upcontrol.io/" and "UPCONTROL.IO" all mean the same address:
 * a missing scheme gets https://, a single trailing slash goes. How the
 * address was typed may not be the reason a check fails to create — the
 * probe needs a URL, the human typed a name.
 */
export function normalizeTarget(raw: string): string {
	let value = raw.trim();
	if (!value) return value;
	if (!/^https?:\/\//i.test(value)) value = 'https://' + value;
	return value.replace(/\/$/, '');
}

/**
 * The discovery door wants a bare host: whatever spelling arrived — scheme,
 * path, trailing slash — collapses to the hostname.
 */
export function normalizeHost(raw: string): string {
	return raw
		.trim()
		.replace(/^https?:\/\//i, '')
		.replace(/\/.*$/, '');
}
