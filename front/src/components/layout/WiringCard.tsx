import { Link } from 'react-router-dom';
import { Skeleton, StatusDot } from '@/components/primitives';
import { channels as channelsApi, explainPreview } from '@/lib/client';
import { useApiData } from '@/lib/useApiData';
import styles from './WiringCard.module.css';

/**
 * What this instance is actually wired to, in the sidebar's dead middle — the
 * self-host answer to the commercial app's plan card. It is the one question a
 * self-hoster keeps re-opening Settings to check.
 *
 * Two rows, and only two. Explain's brain comes from the preview endpoint,
 * which composes the prompt without spending a call, so `model: null` is the
 * off fact rather than a guess. Telegram's presence is read the same way the
 * Settings screen reads it: the server offers a telegram destination only when
 * a bot is configured. There is deliberately NO email-relay row — SMTP is
 * write-only, no GET exists for it, and a row that cannot be read honestly is
 * a row that would have to be invented.
 *
 * Both keys are the ones Settings already fetches, and `useApiData` dedupes by
 * key, so this card costs no extra request.
 */
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
