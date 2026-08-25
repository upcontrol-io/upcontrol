import { useState, type CSSProperties } from 'react';
import { NavLink, useLocation } from 'react-router-dom';
import { ErrorIcon, MoreIcon, PulseIcon, SendIcon, TerminalIcon } from '@/icons';
import { MORE_NAV, PRIMARY_NAV } from './appNav';
import { MoreSheet } from './MoreSheet';
import styles from './BottomTabBar.module.css';

const TAB_ICONS = [PulseIcon, ErrorIcon, TerminalIcon, SendIcon];

/** Phone-width primary navigation: below 700px the sidebar is replaced,
 *  navigation under the thumb; the rest lives one tap away, behind More. */
export function BottomTabBar() {
	const [moreOpen, setMoreOpen] = useState(false);
	const { pathname } = useLocation();

	const moreActive = MORE_NAV.some((item) => pathname.startsWith(item.to));

	const tabIndex = moreActive
		? PRIMARY_NAV.length
		: PRIMARY_NAV.findIndex((item) => pathname.startsWith(item.to));

	return (
		<>
			<nav
				className={[styles.bar, 'uc-glass'].join(' ')}
				aria-label="Primary"
				style={{ '--tab-index': Math.max(0, tabIndex) } as CSSProperties}
			>
				<span
					className={[styles.indicator, tabIndex < 0 && styles.indicatorHidden].filter(Boolean).join(' ')}
					aria-hidden="true"
				/>
				{PRIMARY_NAV.map((item, index) => {
					const Icon = TAB_ICONS[index];
					return (
						<NavLink
							key={item.to}
							to={item.to}
							className={({ isActive }) => [styles.item, isActive && styles.itemActive].filter(Boolean).join(' ')}
						>
							<Icon width={20} height={20} />
							<span className={styles.label}>{item.label}</span>
						</NavLink>
					);
				})}
				<button
					type="button"
					className={[styles.item, moreActive && styles.itemActive].filter(Boolean).join(' ')}
					aria-haspopup="dialog"
					aria-expanded={moreOpen}
					onClick={() => setMoreOpen(true)}
				>
					<MoreIcon width={20} height={20} />
					<span className={styles.label}>More</span>
				</button>
			</nav>
			<MoreSheet open={moreOpen} onClose={() => setMoreOpen(false)} />
		</>
	);
}
