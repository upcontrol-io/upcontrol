import { NavLink, Outlet } from 'react-router-dom';
import { ThemeToggle } from '@/components/primitives';
import { SettingsIcon } from '@/icons';
import {
	PRIMARY_NAV,
	PRIMARY_NAV_LABEL,
	SECONDARY_NAV,
	SECONDARY_NAV_LABEL,
	SETTINGS_NAV,
	type AppNavItem,
} from './appNav';
import { BottomTabBar } from './BottomTabBar';
import { WiringCard } from './WiringCard';
import { Wordmark } from './Wordmark';
import styles from './AppShell.module.css';

/** The self-host app frame: the animated brand lockup (mark + bitmap word,
 *  typewriter on hover — the same header every surface of the product
 *  wears), a grouped nav, content. Prefix matching on NavLink is wanted here —
 *  /monitors/{id} keeps Monitors current.
 *
 *  Two groups above (the moment-to-moment feed, then configuration) and
 *  Settings pinned below a spacer — the commercial app's "Project" group
 *  plus a settings door, minus the Workspace group this single-project
 *  instance has no use for (docs/rules/app.md).
 *
 *  `uc-app` scopes global.css's phone touch-target floor (44px on every
 *  button/[role=button] below 700px) to this tree, the same way the
 *  commercial app's AccountShell does — without it every hand-styled
 *  control in this app ships its desktop size to the phone (rehearsal-driven
 *  mobile pass, 2026-08-20). */
/** One run of nav links under its own label. The two groups used to be told
 *  apart by a 20px gap and nothing else, which reads as a stray space rather
 *  than as a boundary. A word costs less than the bordered group cards the
 *  commercial sidebar wears, and four items do not need that much chrome. */
function NavGroup({ label, items }: { label: string; items: AppNavItem[] }) {
	return (
		<div className={styles.navGroup}>
			<span className={styles.navGroupLabel}>{label}</span>
			{items.map((item) => (
				<NavLink
					key={item.to}
					to={item.to}
					className={({ isActive }) =>
						[styles.navItem, isActive && styles.navItemActive].filter(Boolean).join(' ')
					}
				>
					{item.label}
				</NavLink>
			))}
		</div>
	);
}

export function AppShell() {
	return (
		<div className={[styles.shell, 'uc-app'].join(' ')}>
			<aside className={styles.sidebar}>
				<div className={styles.brandRow}>
					<Wordmark className={styles.wordmark} size={16} />
					<ThemeToggle />
				</div>
				<nav className={styles.nav} aria-label="Application">
					<NavGroup label={PRIMARY_NAV_LABEL} items={PRIMARY_NAV} />
					<NavGroup label={SECONDARY_NAV_LABEL} items={SECONDARY_NAV} />
				</nav>
				<div className={styles.navSpacer} />
				<div className={styles.wiring}>
					<WiringCard />
				</div>
				<NavLink
					to={SETTINGS_NAV.to}
					className={({ isActive }) =>
						[styles.settingsItem, isActive && styles.navItemActive].filter(Boolean).join(' ')
					}
				>
					<SettingsIcon width={16} height={16} />
					{SETTINGS_NAV.label}
				</NavLink>
			</aside>
			<main className={styles.content}>
				<Outlet />
			</main>
			<BottomTabBar />
		</div>
	);
}
