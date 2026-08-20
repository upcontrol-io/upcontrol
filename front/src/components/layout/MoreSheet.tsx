import { createPortal } from 'react-dom';
import { NavLink } from 'react-router-dom';
import { useDismissible } from '@/lib/useDismissible';
import { MORE_NAV } from './appNav';
import styles from './MoreSheet.module.css';

interface MoreSheetProps {
	open: boolean;
	onClose: () => void;
}

/**
 * The bottom sheet behind the phone tab bar's More cell — the sections with
 * no cell of their own: Sources, Status, Settings. A sheet rather than
 * `Modal` because Modal is a centered confirmation panel; this is
 * navigation, anchored to the thumb like the bar that opened it.
 */
export function MoreSheet({ open, onClose }: MoreSheetProps) {
	useDismissible(open, onClose);

	if (!open) return null;

	return createPortal(
		<div className={styles.overlay} onClick={onClose}>
			<div
				role="dialog"
				aria-modal="true"
				aria-label="More"
				className={[styles.sheet, 'uc-glass'].join(' ')}
				onClick={(event) => event.stopPropagation()}
			>
				<span className={styles.grabber} aria-hidden="true" />
				<nav className={styles.nav}>
					{MORE_NAV.map((item) => (
						<NavLink
							key={item.to}
							to={item.to}
							onClick={onClose}
							className={({ isActive }) =>
								[styles.navItem, isActive && styles.navItemActive].filter(Boolean).join(' ')
							}
						>
							{item.label}
						</NavLink>
					))}
				</nav>
			</div>
		</div>,
		document.body,
	);
}
