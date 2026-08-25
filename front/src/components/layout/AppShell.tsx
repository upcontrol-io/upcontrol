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

/** The self-host app frame: brand lockup, grouped nav, content. Prefix
 *  matching on NavLink is wanted: /monitors/{id} keeps Monitors current. */

/** One run of nav links under its own label, so a group boundary reads as a
 *  boundary, not a stray gap. */
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
		<div className={[styles.shell, 'uc-app'].join(' ')}> {/* scopes global.css's phone touch floor */}
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
