/** A bare domain gets https:// and loses its trailing slash: the probe needs
 *  a URL; the human typed a name. */
export function normalizeTarget(raw: string): string {
	let value = raw.trim();
	if (!value) return value;
	if (!/^https?:\/\//i.test(value)) value = 'https://' + value;
	return value.replace(/\/$/, '');
}

/** Collapses any spelling of an address to the hostname the discovery door wants. */
export function normalizeHost(raw: string): string {
	return raw
		.trim()
		.replace(/^https?:\/\//i, '')
		.replace(/\/.*$/, '');
}
