import { useEffect, useRef, useState } from 'react';
import { PageHeader } from '@/components/layout';
import { Button, Callout, Input, LoadError, SkeletonPanel } from '@/components/primitives';
import type { AlertChannel } from '@/lib/types';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import { channels as channelsApi, channelsWrite, delivery, type components } from '@/lib/client';
import styles from './Channels.module.css';

type ChannelsResponse = components['schemas']['ChannelsResponse'];
type Connectable = ChannelsResponse['connectableChannels'][number];

/** The server's words when it sent any; a fixed honest sentence otherwise. */
function writeError(err: unknown, fallback: string): string {
	const message = err instanceof Error ? err.message : '';
	const fromServer = message !== '' && !message.startsWith('HTTP ') && message !== 'unauthorized';
	return fromServer ? message : fallback;
}

/**
 * A channel is a destination and nothing else: one field to add, a Send test
 * per row, an inline-confirm delete. No per-channel rule matrix. A kind the
 * server does not offer gets no tile — a control that cannot act must not
 * exist.
 */
export function Channels() {
	const { loading, failed, data } = useApiData('channels', () => channelsApi());
	const response = data as ChannelsResponse | undefined;

	const [error, setError] = useState('');
	const [removingId, setRemovingId] = useState<string | null>(null);
	// One outcome line per row. `done` marks a settled outcome (sent, dead, or
	// the poll cap) so the row's button reads "Send test" again.
	const [tests, setTests] = useState<Record<string, { text: string; done: boolean }>>({});
	// Adding: which kind's tile is expanded, and the one field's draft.
	const [openKind, setOpenKind] = useState<string | null>(null);
	const [draft, setDraft] = useState('');

	// Poll timers by channel id, cleared on unmount — an unmounted page must
	// not keep asking about a delivery nobody is looking at.
	const timers = useRef<Record<string, ReturnType<typeof setTimeout>>>({});
	useEffect(() => {
		const held = timers.current;
		return () => {
			for (const key of Object.keys(held)) clearTimeout(held[key]);
		};
	}, []);

	if (loading) {
		return <SkeletonPanel rows={3} label="Loading channels" />;
	}
	if (failed || !response) {
		return <LoadError what="your channels" onRetry={() => invalidateApiData('channels')} />;
	}

	function sendTest(channel: AlertChannel) {
		clearTimeout(timers.current[channel.id]);
		// Queued is not sent: the row says what is actually known, and the
		// outcome comes from the delivery queue's own record.
		setTests((cur) => ({ ...cur, [channel.id]: { text: 'queued, waiting for the outcome', done: false } }));
		void channelsWrite
			.test(channel.id)
			.then((queued) => {
				let polls = 0;
				const poll = () => {
					polls += 1;
					void delivery(queued.id)
						.then((status) => {
							if (status.state === 'sent') {
								setTests((cur) => ({ ...cur, [channel.id]: { text: 'sent', done: true } }));
							} else if (status.state === 'dead') {
								setTests((cur) => ({
									...cur,
									[channel.id]: { text: status.deadReason || 'died in the queue', done: true },
								}));
							} else if (polls >= 15) {
								setTests((cur) => ({ ...cur, [channel.id]: { text: 'the outcome is not known yet', done: true } }));
							} else {
								timers.current[channel.id] = setTimeout(poll, 2000);
							}
						})
						.catch(() => {
							setTests((cur) => ({ ...cur, [channel.id]: { text: 'the outcome is not known yet', done: true } }));
						});
				};
				timers.current[channel.id] = setTimeout(poll, 2000);
			})
			.catch((err: unknown) => {
				setTests((cur) => ({
					...cur,
					[channel.id]: { text: writeError(err, 'the test was not queued'), done: true },
				}));
			});
	}

	function removeChannel(id: string) {
		setRemovingId(null);
		void channelsWrite
			.delete(id)
			.then(() => {
				setError('');
				invalidateApiData('channels');
			})
			.catch((err: unknown) => {
				setError(writeError(err, 'Could not remove that destination. It is still there. Try again.'));
			});
	}

	function addChannel(connectable: Connectable) {
		const target = draft.trim();
		if (!target) return;
		void channelsWrite
			.create(connectable.kind, target)
			.then(() => {
				setError('');
				setOpenKind(null);
				setDraft('');
				invalidateApiData('channels');
			})
			.catch((err: unknown) => {
				setError(writeError(err, 'Could not add that destination. Nothing was saved.'));
			});
	}

	return (
		<section className={styles.page}>
			<PageHeader
				title="Channels"
				description="A channel is a destination. Every alert goes to all of them."
			/>

			{response.undelivered > 0 && (
				<Callout tone="danger" title="Deliveries are dying">
					{response.undelivered} alert deliveries died in the queue over the last 24 hours.
				</Callout>
			)}

			{error && (
				<Callout tone="danger" title="That did not save">
					{error}
				</Callout>
			)}

			{response.channels.length > 0 && (
				<div className={styles.list}>
					{response.channels.map((channel) => {
						const test = tests[channel.id];
						return (
							<div key={channel.id} className={styles.row}>
								<div className={styles.rowMain}>
									<span className={styles.kind}>{channel.kind}</span>
									<div className={styles.targetCell}>
										<span className={styles.target}>{channel.target}</span>
										{channel.note && <span className={styles.note}>{channel.note}</span>}
										{test && <span className={styles.testOutcome}>{test.text}</span>}
									</div>
									<Button
										variant="secondary"
										size="sm"
										disabled={Boolean(test) && !test.done}
										onClick={() => sendTest(channel)}
									>
										Send test
									</Button>
									<button
										type="button"
										className={styles.remove}
										aria-label={`Remove ${channel.target}`}
										onClick={() => setRemovingId((cur) => (cur === channel.id ? null : channel.id))}
									>
										×
									</button>
								</div>
								{removingId === channel.id && (
									<div className={styles.confirmStrip}>
										<span className={styles.confirmText}>Remove this destination?</span>
										<Button variant="danger" size="sm" onClick={() => removeChannel(channel.id)}>
											Delete
										</Button>
										<Button variant="ghost" size="sm" onClick={() => setRemovingId(null)}>
											Keep
										</Button>
									</div>
								)}
							</div>
						);
					})}
				</div>
			)}

			<div className={styles.connectables}>
				{response.connectableChannels.map((connectable) => (
					<div key={connectable.kind} className={styles.tile}>
						<button
							type="button"
							className={styles.tileHead}
							onClick={() => {
								setOpenKind((cur) => (cur === connectable.kind ? null : connectable.kind));
								setDraft('');
							}}
						>
							<span className={styles.tileName}>{connectable.name}</span>
							<span className={styles.tileHint}>{connectable.hint}</span>
						</button>
						{openKind === connectable.kind &&
							(connectable.link ? (
								// Telegram: a chat id cannot be typed into a form — the bot
								// binds the chat when it hears from you.
								<div className={styles.tileBody}>
									<a href={connectable.link} target="_blank" rel="noreferrer" className={styles.tileLink}>
										Open Telegram
									</a>
								</div>
							) : (
								<form
									className={styles.tileBody}
									onSubmit={(event) => {
										event.preventDefault();
										addChannel(connectable);
									}}
								>
									<Input
										label={connectable.field}
										placeholder={connectable.placeholder}
										value={draft}
										onChange={(event) => setDraft(event.target.value)}
									/>
									<Button type="submit" disabled={!draft.trim()}>
										Add
									</Button>
								</form>
							))}
					</div>
				))}
			</div>
		</section>
	);
}
