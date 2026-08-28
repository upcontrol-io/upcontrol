import { Link } from 'react-router-dom';
import { Skeleton, StatusDot } from '@/components/primitives';
import { channels as channelsApi } from '@/lib/client';
import { useApiData } from '@/lib/useApiData';
import styles from './WiringCard.module.css';

/** What this instance is wired to. The server offers a telegram destination
 *  only with a bot configured, so the channels read IS the presence fact; no
 *  email-relay row exists because SMTP has no honest GET. */
export function WiringCard() {
	const { data: chans, loading: chansLoading } = useApiData('channels', () => channelsApi());

	const telegramOn =
		(chans?.connectableChannels ?? []).some((c) => c.kind === 'telegram') ||
		(chans?.channels ?? []).some((c) => c.kind === 'telegram');

	return (
		<Link to="/settings" className={styles.card}>
			<span className={styles.head}>Wired up</span>
			<span className={styles.row}>
				<span className={styles.label}>Telegram</span>
				{chansLoading ? (
					// Nothing has answered yet. A dot here would be a claim.
					<Skeleton width={52} height={10} />
				) : (
					// Never colour alone: the dot carries a word beside it.
					<StatusDot
						status={telegramOn ? 'ok' : 'nodata'}
						label={telegramOn ? 'on' : 'not set'}
						className={styles.state}
					/>
				)}
			</span>
		</Link>
	);
}
