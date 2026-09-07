import { useEffect, useState } from 'react';
import { PageHeader } from '@/components/layout';
import { Button, Callout, Input, LoadError, Modal, SkeletonPanel } from '@/components/primitives';
import { CopyField } from '@/components/code';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import {
	channels as channelsApi,
	createKey,
	installToken as installTokenApi,
	instance,
	keys as keysApi,
	revokeKey,
	rotateKey,
	statusPage as statusPageApi,
} from '@/lib/client';
import styles from './Settings.module.css';

/** How long a rotated key keeps working, so a deployed app can catch up. */
const ROTATE_OVERLAP = '24 hours';

/** One modal asks every key question; after an issue it flips to the copy view. */
type KeyAsk = { kind: 'add' } | { kind: 'rotate' } | { kind: 'revoke'; id: string; label: string };

const whenFmt = new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'short' });

function useSectionAction() {
	const [busy, setBusy] = useState(false);
	const [note, setNote] = useState<{ text: string; failed: boolean } | null>(null);
	async function run(fn: () => Promise<string>, fallback: string, useServerMessage = true) {
		if (busy) return;
		setBusy(true);
		setNote(null);
		try {
			setNote({ text: await fn(), failed: false });
		} catch (err) {
			// The server's own refusal names the fix; anything else is the
			// generic transport line.
			setNote({
				text: useServerMessage && err instanceof Error && err.message !== 'unauthorized' ? err.message : fallback,
				failed: true,
			});
		} finally {
			setBusy(false);
		}
	}
	return { busy, note, run };
}

/** The instance's few real knobs. Project name is the status page's title (the
 *  only name with a write behind it); the ingest keys arrive via install token. */
export function Settings() {
	const { data: page, live: pageLive, loading: pageLoading, failed: pageFailed } = useApiData(
		'statusPage',
		() => statusPageApi.get(),
	);
	const { data: liveKeys, loading: keysLoading, failed: keysFailed } = useApiData('keys', () => keysApi());
	// The bot's presence fact: the server offers a telegram destination only
	// when a bot is configured, so the channels read doubles as the indicator.
	const { data: chans } = useApiData('channels', () => channelsApi());

	const [name, setName] = useState('');
	const [error, setError] = useState('');
	// The generated install command. Created by an explicit click, never on
	// render — a token per page view would mint credentials nobody asked for.
	const [installCmd, setInstallCmd] = useState<{ command: string; expiresAt: string } | null>(null);
	const [tokenBusy, setTokenBusy] = useState(false);
	const [tokenError, setTokenError] = useState<string | null>(null);
	// One modal asks every key question — add, rotate, revoke — and after an
	// issue flips to the one-time copy view. The full key lives in state
	// only while that view is open.
	const [keyAsk, setKeyAsk] = useState<KeyAsk | null>(null);
	const [newKeyName, setNewKeyName] = useState('');
	const [fullKey, setFullKey] = useState<string | null>(null);
	const keyAction = useSectionAction();
	// Write-only fields: the server seals what is typed here and never
	// returns it, so these inputs always start empty.
	const [tgToken, setTgToken] = useState('');
	const [tgUsername, setTgUsername] = useState('');
	const tg = useSectionAction();
	const [smtpHost, setSmtpHost] = useState('');
	const [smtpPort, setSmtpPort] = useState('');
	const [smtpUsername, setSmtpUsername] = useState('');
	const [smtpPassword, setSmtpPassword] = useState('');
	const [smtpFrom, setSmtpFrom] = useState('');
	const smtp = useSectionAction();
	const [smtpRemoveAsking, setSmtpRemoveAsking] = useState(false);

	useEffect(() => {
		if (page) setName(page.title ?? '');
	}, [page]);

	// Declared once, rendered by every branch (loading, failed, empty, live).
	const header = <PageHeader title="Settings" description="This instance's few real knobs: the project name, the ingest keys, and the services it talks to." />;

	if (pageLoading || keysLoading) {
		return (
			<div className={styles.wrap}>
				{header}
				<SkeletonPanel rows={3} label="Loading settings" />
			</div>
		);
	}
	if (pageFailed || keysFailed || !page || !liveKeys) {
		return (
			<div className={styles.wrap}>
				{header}
				<LoadError what="your settings" onRetry={() => invalidateApiData('statusPage', 'keys')} />
			</div>
		);
	}

	function saveName() {
		const next = name.trim();
		if (!pageLive || !page || next === (page.title ?? '')) return;
		void statusPageApi
			.put({
				title: next,
				domain: page.domain ?? '',
				shown: Object.fromEntries(page.components.map((c) => [c.key, c.shown])),
				showNetwork: page.showNetwork,
				showPoweredBy: page.showPoweredBy !== false,
			})
			.then(() => {
				setError('');
				invalidateApiData('statusPage');
			})
			.catch(() => setError('The name did not save. Nothing changed.'));
	}

	async function generateCommand() {
		if (tokenBusy) return;
		setTokenBusy(true);
		setTokenError(null);
		try {
			const t = await installTokenApi();
			setInstallCmd({ command: t.command, expiresAt: t.expiresAt });
		} catch {
			setTokenError('Could not create a command. Try again.');
		} finally {
			setTokenBusy(false);
		}
	}

	const tgReady =
		(chans?.connectableChannels ?? []).some((c) => c.kind === 'telegram') ||
		(chans?.channels ?? []).some((c) => c.kind === 'telegram');

	async function saveTelegramBot() {
		const token = tgToken.trim();
		const username = tgUsername.trim();
		if (!token || !username) return;
		await tg.run(
			async () => {
				await instance.putTelegramBot(token, username);
				setTgToken('');
				setTgUsername('');
				invalidateApiData('channels');
				return 'Saved. Alerts and invites work now; the bot starts polling within a minute, and Mini App sign-in joins after the next restart.';
			},
			'Could not save the bot. Try again.',
		);
	}

	async function saveSMTP() {
		const values: { host?: string; port?: string; username?: string; password?: string; from?: string } = {};
		if (smtpHost.trim()) values.host = smtpHost.trim();
		if (smtpPort.trim()) values.port = smtpPort.trim();
		if (smtpUsername.trim()) values.username = smtpUsername.trim();
		if (smtpPassword.trim()) values.password = smtpPassword.trim();
		if (smtpFrom.trim()) values.from = smtpFrom.trim();
		if (Object.keys(values).length === 0) return;
		await smtp.run(
			async () => {
				await instance.putSMTP(values);
				setSmtpHost('');
				setSmtpPort('');
				setSmtpUsername('');
				setSmtpPassword('');
				setSmtpFrom('');
				return 'Saved. Sign-in mail and email alerts use this relay from the next send on.';
			},
			'Could not save. Try again.',
		);
	}

	async function removeSMTP() {
		await smtp.run(
			async () => {
				await instance.deleteSMTP();
				return 'Removed. If the server env still carries SMTP settings, mail keeps using those; otherwise sign-in codes land in the ucapi log.';
			},
			'Could not remove the settings. Try again.',
			false,
		);
		setSmtpRemoveAsking(false);
	}

	async function runKeyAsk() {
		if (!keyAsk || keyAction.busy) return;
		const ask = keyAsk;
		let done = false;
		await keyAction.run(
			async () => {
				if (ask.kind === 'add') {
					setFullKey((await createKey(newKeyName.trim())).value);
					setNewKeyName('');
				} else if (ask.kind === 'rotate') {
					setFullKey((await rotateKey()).value);
				} else {
					await revokeKey(ask.id);
				}
				done = true;
				invalidateApiData('keys');
				return ask.kind === 'revoke'
					? `Revoked. ${ask.label} stopped being accepted at once; its row stays as the record of what it reached.`
					: `${ask.kind === 'add' ? 'Added' : 'Rotated'}. The full key was shown once — it cannot be shown again.`;
			},
			ask.kind === 'add'
				? 'Could not add the key. Try again.'
				: ask.kind === 'rotate'
					? 'Could not rotate. Try again.'
					: 'Could not revoke. Try again.',
		);
		// A revoke has nothing to copy; a failure closes the ask so the
		// server's own refusal (the 409 names the fix) is readable below.
		if (!done || ask.kind === 'revoke') setKeyAsk(null);
	}

	return (
		<div className={styles.wrap}>
			{header}

			{error && (
				<Callout tone="danger" title="That did not save">
					{error}
				</Callout>
			)}

			<section className={styles.section}>
				<h2 className={styles.sectionTitle}>Project name</h2>
				<span className={styles.hint}>Names the public status page. There is nothing else a name changes.</span>
				<form
					className={styles.nameRow}
					onSubmit={(event) => {
						event.preventDefault();
						saveName();
					}}
				>
					<Input value={name} onChange={(event) => setName(event.target.value)} placeholder="My product" />
					<Button type="submit" variant="secondary" disabled={name.trim() === (page.title ?? '')}>
						Save
					</Button>
				</form>
			</section>

			<section className={styles.section}>
				<h2 className={styles.sectionTitle}>Ingest keys</h2>
				{/* Write-only is a trust argument: a key that leaks out of a repo
				    can only send, never read. */}
				<span className={styles.hint}>
					Write-only — they can send data, never read it. Only each prefix is kept, an identifier
					not a credential; the full key is shown once, when it is issued.
				</span>
				{liveKeys.keys.length === 0 ? (
					<span className={styles.hint}>No keys yet. Add one to start sending data in.</span>
				) : (
					<ul className={styles.keyList}>
						{liveKeys.keys.map((k) => (
							<li
								key={k.id}
								className={k.state === 'revoked' ? `${styles.keyRow} ${styles.keyRowOff}` : styles.keyRow}
							>
								<span className={styles.keyName}>{k.name || k.prefix}</span>
								{k.state !== 'active' && (
									<span className={styles.keyFlag}>
										{k.state === 'revoked' ? 'revoked' : `replaced — works ${ROTATE_OVERLAP} longer`}
									</span>
								)}
								<span className={styles.keyMeta}>
									{k.name ? `${k.prefix} · ` : ''}Created {whenFmt.format(new Date(k.createdAt))} ·{' '}
									{k.lastUsedAt ? `Last used ${whenFmt.format(new Date(k.lastUsedAt))}` : 'Never used'}
								</span>
								{k.state !== 'revoked' && (
									<Button
										variant="ghost"
										size="sm"
										className={styles.keyRevoke}
										disabled={keyAction.busy}
										onClick={() => setKeyAsk({ kind: 'revoke', id: k.id, label: k.name || k.prefix })}
									>
										Revoke
									</Button>
								)}
							</li>
						))}
					</ul>
				)}
				{keyAction.note && (
					<span className={keyAction.note.failed ? styles.tokenError : styles.hint}>{keyAction.note.text}</span>
				)}

				<span className={styles.stepLabel}>Wire the SDK — one command, whatever agent you use</span>
				{installCmd ? (
					<>
						<CopyField text={installCmd.command} />
						<span className={styles.hint}>
							One-time token, expires in 10 minutes — it lands this project's key in a gitignored .env
							without ever showing it.{' '}
							<button type="button" className={styles.linkButton} onClick={generateCommand}>
								{tokenBusy ? 'Generating…' : 'Generate a new one'}
							</button>
						</span>
					</>
				) : (
					<Button variant="primary" onClick={generateCommand}>
						{tokenBusy ? 'Generating…' : 'Generate install command'}
					</Button>
				)}
				{tokenError && <span className={styles.tokenError}>{tokenError}</span>}

				<div className={styles.rotateRow}>
					<Button variant="secondary" size="sm" onClick={() => setKeyAsk({ kind: 'add' })}>
						Add a key
					</Button>
					<Button variant="secondary" size="sm" onClick={() => setKeyAsk({ kind: 'rotate' })}>
						Rotate every key
					</Button>
					<span className={styles.hint}>
						Leaked? Rotate replaces every working key at once. One key gone bad: revoke that row
						and add a replacement.
					</span>
				</div>
			</section>

			<section className={styles.section}>
				<h2 className={styles.sectionTitle}>Telegram bot</h2>
				{tgReady ? (
					<span className={styles.hint}>
						A bot is connected — Telegram destinations and invites are live on the Channels screen.
					</span>
				) : (
					<span className={styles.hint}>
						No bot yet. Create one with <code>@BotFather</code> in Telegram (takes a minute), then paste
						what it gives you here.
					</span>
				)}
				<form
					className={styles.nameRow}
					onSubmit={(event) => {
						event.preventDefault();
						void saveTelegramBot();
					}}
				>
					<Input
						type="password"
						value={tgToken}
						onChange={(event) => setTgToken(event.target.value)}
						placeholder="123456789:AA…"
						aria-label="Telegram bot token"
						autoComplete="off"
					/>
					<Input
						value={tgUsername}
						onChange={(event) => setTgUsername(event.target.value)}
						placeholder="my_alerts_bot"
						aria-label="Telegram bot username"
						autoComplete="off"
					/>
					<Button type="submit" variant="secondary" disabled={!tgToken.trim() || !tgUsername.trim() || tg.busy}>
						{tg.busy ? 'Saving…' : 'Save bot'}
					</Button>
				</form>
				<span className={styles.hint}>
					The token exactly as @BotFather printed it, and the bot's username (without @) — it makes the{' '}
					<code>t.me</code> links. Both are stored encrypted and never shown again.
				</span>
				{tg.note && <span className={tg.note.failed ? styles.tokenError : styles.hint}>{tg.note.text}</span>}
			</section>

			<section className={styles.section}>
				<h2 className={styles.sectionTitle}>Email relay</h2>
				<span className={styles.hint}>
					SMTP for sign-in mail and email alerts. Without a relay, sign-in codes land in the ucapi log
					instead of an inbox — fine on your own machine, not for anyone else's.
				</span>
				<form
					className={styles.stackForm}
					onSubmit={(event) => {
						event.preventDefault();
						void saveSMTP();
					}}
				>
					<Input
						value={smtpHost}
						onChange={(event) => setSmtpHost(event.target.value)}
						placeholder="Host: smtp.eu.mailgun.org"
						aria-label="SMTP host"
						autoComplete="off"
					/>
					<Input
						value={smtpPort}
						onChange={(event) => setSmtpPort(event.target.value)}
						placeholder="Port: 587"
						aria-label="SMTP port"
						autoComplete="off"
					/>
					<Input
						value={smtpUsername}
						onChange={(event) => setSmtpUsername(event.target.value)}
						placeholder="Username"
						aria-label="SMTP username"
						autoComplete="off"
					/>
					<Input
						type="password"
						value={smtpPassword}
						onChange={(event) => setSmtpPassword(event.target.value)}
						placeholder="Password"
						aria-label="SMTP password"
						autoComplete="off"
					/>
					<Input
						value={smtpFrom}
						onChange={(event) => setSmtpFrom(event.target.value)}
						placeholder="From address: alerts@example.com"
						aria-label="SMTP from address"
						autoComplete="off"
					/>
					<Button
						type="submit"
						variant="secondary"
						disabled={
							smtp.busy ||
							(!smtpHost.trim() && !smtpPort.trim() && !smtpUsername.trim() && !smtpPassword.trim() && !smtpFrom.trim())
						}
					>
						{smtp.busy ? 'Saving…' : 'Save email relay'}
					</Button>
				</form>
				<span className={styles.hint}>
					Any relay works (Mailgun, SES, Postmark, your own Postfix). Fill only what you are changing —
					empty fields keep their current value. Everything is stored encrypted and never shown again.
				</span>
				{smtp.note && <span className={smtp.note.failed ? styles.tokenError : styles.hint}>{smtp.note.text}</span>}
				{/* Unconditional: SMTP is write-only, nothing reads its state back,
				    so a DELETE-on-nothing no-op is the honest option. */}
				<div className={styles.rotateRow}>
					{smtpRemoveAsking ? (
						<>
							<span className={styles.hint}>Remove the email relay saved here?</span>
							<Button variant="danger" size="sm" disabled={smtp.busy} onClick={() => void removeSMTP()}>
								Remove
							</Button>
							<Button variant="ghost" size="sm" onClick={() => setSmtpRemoveAsking(false)}>
								Keep
							</Button>
						</>
					) : (
						<Button variant="secondary" size="sm" onClick={() => setSmtpRemoveAsking(true)}>
							Remove email relay
						</Button>
					)}
				</div>
			</section>

			<p className={styles.docsLink}>
				Hook and SDK guides live at{' '}
				<a href="https://upcontrol.io/docs" target="_blank" rel="noreferrer">
					upcontrol.io/docs
				</a>
				.
			</p>

			<Modal
				open={keyAsk !== null || fullKey !== null}
				onClose={() => {
					setKeyAsk(null);
					setFullKey(null);
				}}
				title={
					fullKey
						? 'Copy your new key'
						: keyAsk?.kind === 'add'
							? 'Add a key'
							: keyAsk?.kind === 'rotate'
								? 'Rotate every key?'
								: 'Revoke this key?'
				}
			>
				{fullKey ? (
					<>
						{/* Not "the old key stops immediately": a rotation that breaks a
						    deployed app fails at the customer. */}
						<p className={styles.modalWarning}>
							Copy it now — this is the only time the full key is shown.
							{keyAsk?.kind === 'rotate' && <> The old ones keep working for {ROTATE_OVERLAP}, then stop.</>}
						</p>
						<CopyField text={fullKey} />
						<div className={styles.modalActions}>
							<Button
								variant="primary"
								onClick={() => {
									setKeyAsk(null);
									setFullKey(null);
								}}
							>
								Done
							</Button>
						</div>
					</>
				) : keyAsk?.kind === 'add' ? (
					<form
						onSubmit={(event) => {
							event.preventDefault();
							void runKeyAsk();
						}}
					>
						<Input
							value={newKeyName}
							onChange={(event) => setNewKeyName(event.target.value)}
							placeholder="staging"
							aria-label="Key name"
							autoComplete="off"
						/>
						<p className={styles.modalWarning}>
							A name tells two keys apart by something other than their prefix. Optional.
						</p>
						<div className={styles.modalActions}>
							<Button type="button" variant="ghost" disabled={keyAction.busy} onClick={() => setKeyAsk(null)}>
								Cancel
							</Button>
							<Button type="submit" variant="primary" disabled={keyAction.busy}>
								{keyAction.busy ? 'Adding…' : 'Add key'}
							</Button>
						</div>
					</form>
				) : keyAsk?.kind === 'rotate' ? (
					<>
						<p className={styles.modalWarning}>
							Every working key is replaced at once. Each old one keeps working for {ROTATE_OVERLAP}, so
							anything already deployed has time to pick up the new key — after that it stops.
						</p>
						<div className={styles.modalActions}>
							<Button variant="ghost" disabled={keyAction.busy} onClick={() => setKeyAsk(null)}>
								Cancel
							</Button>
							<Button variant="primary" disabled={keyAction.busy} onClick={() => void runKeyAsk()}>
								{keyAction.busy ? 'Rotating…' : 'Rotate every key'}
							</Button>
						</div>
					</>
				) : keyAsk?.kind === 'revoke' ? (
					<>
						<p className={styles.modalWarning}>
							Revoke {keyAsk.label}? It stops being accepted at once — no overlap window. The row stays
							listed, its last use still readable.
						</p>
						<div className={styles.modalActions}>
							<Button variant="ghost" disabled={keyAction.busy} onClick={() => setKeyAsk(null)}>
								Keep
							</Button>
							<Button variant="danger" disabled={keyAction.busy} onClick={() => void runKeyAsk()}>
								{keyAction.busy ? 'Revoking…' : 'Revoke'}
							</Button>
						</div>
					</>
				) : null}
			</Modal>
		</div>
	);
}
