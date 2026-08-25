export interface AppNavItem {
	to: string;
	label: string;
}

/** The moment-to-moment surface: what is watched, what broke, the stream, who
 *  hears about it. The labels below render in the sidebar only. */
export const PRIMARY_NAV_LABEL = 'Watch';
export const SECONDARY_NAV_LABEL = 'Publish';

export const PRIMARY_NAV: AppNavItem[] = [
	{ to: '/monitors', label: 'Monitors' },
	{ to: '/incidents', label: 'Incidents' },
	{ to: '/logs', label: 'Logs' },
	{ to: '/channels', label: 'Channels' },
];

/** Configuration and the public-facing surface — set apart from the feed above. */
export const SECONDARY_NAV: AppNavItem[] = [
	{ to: '/sources', label: 'Sources' },
	{ to: '/status', label: 'Status' },
];

export const SETTINGS_NAV: AppNavItem = { to: '/settings', label: 'Settings' };

/** Everything behind the phone tab bar's More sheet: SECONDARY_NAV plus the
 *  settings door that has no cell of its own on a 5-wide bar. */
export const MORE_NAV: AppNavItem[] = [...SECONDARY_NAV, SETTINGS_NAV];
