import { Link } from 'react-router-dom';
import { Skeleton, StatusDot } from '@/components/primitives';
import { channels as channelsApi, explainPreview } from '@/lib/client';
import { useApiData } from '@/lib/useApiData';
import styles from './WiringCard.module.css';

/** What this instance is wired to. `model: null` from the preview endpoint is
 *  the off fact; no email-relay row exists because SMTP has no honest GET. */
export function WiringCard() {
	const { data: brain, loading: brainLoading } = useApiData('aiBrain', () => explainPreview([]));
	const { data: chans, loading: chansLoading } = useApiData('channels', () => channelsApi());

	const explainOn = brain?.model ? true : false;
	const telegramOn =
		(chans?.connectableChannels ?? []).some((c) => c.kind === 'telegram') ||
		(chans?.channels ?? []).some((c) => c.kind === 'telegram');

	const rows: [string, boolean, boolean][] = [
		['Explain', brainLoading, explainOn],
		['Telegram', chansLoading, telegramOn],
	];

	return (
		<Link to="/settings" className={styles.card}>
			<span className={styles.head}>Wired up</span>
			{rows.map(([label, loading, on]) => (
				<span key={label} className={styles.row}>
					<span className={styles.label}>{label}</span>
					{loading ? (
						// Nothing has answered yet. A dot here would be a claim.
						<Skeleton width={52} height={10} />
					) : (
						// Never colour alone: the dot carries a word beside it.
						<StatusDot
							status={on ? 'ok' : 'nodata'}
							label={on ? 'on' : 'not set'}
							className={styles.state}
						/>
					)}
				</span>
			))}
		</Link>
	);
}
