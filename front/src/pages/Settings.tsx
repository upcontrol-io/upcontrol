import { useEffect, useState } from 'react';
import { PageHeader } from '@/components/layout';
import { Button, Callout, Input, LoadError, Modal, SkeletonPanel } from '@/components/primitives';
import { CopyField } from '@/components/code';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import {
	channels as channelsApi,
	explainPreview,
	installToken as installTokenApi,
	instance,
	keys as keysApi,
	rotateKey,
	statusPage as statusPageApi,
} from '@/lib/client';
import styles from './Settings.module.css';

/** How long a rotated key keeps working, so a deployed app can catch up. */
const ROTATE_OVERLAP = '24 hours';

/** The instance's few real knobs. Project name is the status page's title (the
 *  only name with a write behind it); the ingest key arrives via install token. */
export function Settings() {
	const { data: page, live: pageLive, loading: pageLoading, failed: pageFailed } = useApiData(
		'statusPage',
		() => statusPageApi.get(),
	);
	const { data: liveKeys, loading: keysLoading, failed: keysFailed } = useApiData('keys', () => keysApi());
	// The wired brain, read from the preview endpoint — composed, not executed:
	// it spends nothing and consults no model. `model: null` = Explain is off.
	const { data: brain } = useApiData('aiBrain', () => explainPreview([]));
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
	const [rotateOpen, setRotateOpen] = useState(false);
	const [rotating, setRotating] = useState(false);
	// The full key exists only at rotate; this is the one place it is shown.
	const [rotatedKey, setRotatedKey] = useState<string | null>(null);
	// Write-only fields: the server seals what is typed here and never
	// returns it, so these inputs always start empty.
	const [aiKey, setAiKey] = useState('');
	const [aiModelField, setAiModelField] = useState('');
	const [aiBaseURL, setAiBaseURL] = useState('');
	const [aiBusy, setAiBusy] = useState(false);
	const [aiNote, setAiNote] = useState<{ text: string; failed: boolean } | null>(null);
	const [aiRemoveAsking, setAiRemoveAsking] = useState(false);
	const [tgToken, setTgToken] = useState('');
	const [tgUsername, setTgUsername] = useState('');
	const [tgBusy, setTgBusy] = useState(false);
	const [tgNote, setTgNote] = useState<{ text: string; failed: boolean } | null>(null);
	const [smtpHost, setSmtpHost] = useState('');
	const [smtpPort, setSmtpPort] = useState('');
	const [smtpUsername, setSmtpUsername] = useState('');
	const [smtpPassword, setSmtpPassword] = useState('');
	const [smtpFrom, setSmtpFrom] = useState('');
	const [smtpBusy, setSmtpBusy] = useState(false);
	const [smtpNote, setSmtpNote] = useState<{ text: string; failed: boolean } | null>(null);
	const [smtpRemoveAsking, setSmtpRemoveAsking] = useState(false);

	useEffect(() => {
		if (page) setName(page.title ?? '');
	}, [page]);

	// Declared once, rendered by every branch (loading, failed, empty, live).
	const header = <PageHeader title="Settings" description="This instance's few real knobs: the project name, the ingest key, and the services it talks to." />;

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
				showIncidents: page.showIncidents,
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

	// undefined = still reading; null = Explain is off; string = the brain.
	const aiModel = brain === undefined ? undefined : (brain?.model ?? null);
	const tgReady =
		(chans?.connectableChannels ?? []).some((c) => c.kind === 'telegram') ||
		(chans?.channels ?? []).some((c) => c.kind === 'telegram');

	async function saveAI() {
		const values: { key?: string; model?: string; baseUrl?: string } = {};
		if (aiKey.trim()) values.key = aiKey.trim();
		if (aiModelField.trim()) values.model = aiModelField.trim();
		if (aiBaseURL.trim()) values.baseUrl = aiBaseURL.trim();
		if (aiBusy || Object.keys(values).length === 0) return;
		setAiBusy(true);
		setAiNote(null);
		try {
			await instance.putAI(values);
			setAiKey('');
			setAiModelField('');
			setAiBaseURL('');
			invalidateApiData('aiBrain');
			setAiNote({ text: 'Saved. Explain answers with these settings from the next question on.', failed: false });
		} catch (err) {
			// The server's own refusal names the fix; anything else is the
			// generic transport line.
			setAiNote({
				text: err instanceof Error && err.message !== 'unauthorized' ? err.message : 'Could not save. Try again.',
				failed: true,
			});
		} finally {
			setAiBusy(false);
		}
	}

	async function removeAI() {
		if (aiBusy) return;
		setAiBusy(true);
		setAiNote(null);
		try {
			await instance.deleteAI();
			invalidateApiData('aiBrain');
			setAiNote({
				text: 'Removed. If the server env still carries AI settings, Explain keeps using those; otherwise Explain is off.',
				failed: false,
			});
		} catch {
			setAiNote({ text: 'Could not remove the settings. Try again.', failed: true });
		} finally {
			setAiBusy(false);
			setAiRemoveAsking(false);
		}
	}

	async function saveTelegramBot() {
		const token = tgToken.trim();
		const username = tgUsername.trim();
		if (!token || !username || tgBusy) return;
		setTgBusy(true);
		setTgNote(null);
		try {
			await instance.putTelegramBot(token, username);
			setTgToken('');
			setTgUsername('');
			invalidateApiData('channels');
			setTgNote({
				text: 'Saved. Alerts and invites work now; the bot starts polling within a minute, and Mini App sign-in joins after the next restart.',
				failed: false,
			});
		} catch (err) {
			setTgNote({
				text: err instanceof Error && err.message !== 'unauthorized' ? err.message : 'Could not save the bot. Try again.',
				failed: true,
			});
		} finally {
			setTgBusy(false);
		}
	}

	async function saveSMTP() {
		const values: { host?: string; port?: string; username?: string; password?: string; from?: string } = {};
		if (smtpHost.trim()) values.host = smtpHost.trim();
		if (smtpPort.trim()) values.port = smtpPort.trim();
		if (smtpUsername.trim()) values.username = smtpUsername.trim();
		if (smtpPassword.trim()) values.password = smtpPassword.trim();
		if (smtpFrom.trim()) values.from = smtpFrom.trim();
		if (smtpBusy || Object.keys(values).length === 0) return;
		setSmtpBusy(true);
		setSmtpNote(null);
		try {
			await instance.putSMTP(values);
			setSmtpHost('');
			setSmtpPort('');
			setSmtpUsername('');
			setSmtpPassword('');
			setSmtpFrom('');
			setSmtpNote({ text: 'Saved. Sign-in mail and email alerts use this relay from the next send on.', failed: false });
		} catch (err) {
			setSmtpNote({
				text: err instanceof Error && err.message !== 'unauthorized' ? err.message : 'Could not save. Try again.',
				failed: true,
			});
		} finally {
			setSmtpBusy(false);
		}
	}

	async function removeSMTP() {
		if (smtpBusy) return;
		setSmtpBusy(true);
		setSmtpNote(null);
		try {
			await instance.deleteSMTP();
			setSmtpNote({
				text: 'Removed. If the server env still carries SMTP settings, mail keeps using those; otherwise sign-in codes land in the ucapi log.',
				failed: false,
			});
		} catch {
			setSmtpNote({ text: 'Could not remove the settings. Try again.', failed: true });
		} finally {
			setSmtpBusy(false);
			setSmtpRemoveAsking(false);
		}
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
				<h2 className={styles.sectionTitle}>Ingest key</h2>
				{/* Write-only is a trust argument: a key that leaks out of a repo
				    can only send, never read. One project, one key. */}
				<span className={styles.hint}>
					Write-only — it can send data, never read it. Only the prefix is stored; the full key is shown once,
					when you rotate it.
				</span>
				<div className={styles.keyRow}>
					<span className={styles.keyValue}>{liveKeys.key?.prefix ?? '—'}</span>
					<span className={styles.hint}>Key prefix — an identifier, not a credential.</span>
				</div>

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
					<Button variant="secondary" size="sm" onClick={() => setRotateOpen(true)}>
						Rotate key
					</Button>
					<span className={styles.hint}>The manual path: rotate, copy the key once, run npx upcontrol init --key yourself.</span>
				</div>
			</section>

			<section className={styles.section}>
				<h2 className={styles.sectionTitle}>AI</h2>
				{aiModel === undefined ? (
					<span className={styles.hint}>Reading whether Explain is configured…</span>
				) : aiModel === null ? (
					<span className={styles.hint}>Explain is off — no API key is configured.</span>
				) : (
					<span className={styles.hint}>
						Explain is answered by <code>{aiModel}</code>.
					</span>
				)}
				<form
					className={styles.stackForm}
					onSubmit={(event) => {
						event.preventDefault();
						void saveAI();
					}}
				>
					<Input
						type="password"
						value={aiKey}
						onChange={(event) => setAiKey(event.target.value)}
						placeholder="API key: sk-…"
						aria-label="OpenAI-format API key"
						autoComplete="off"
					/>
					<Input
						value={aiModelField}
						onChange={(event) => setAiModelField(event.target.value)}
						placeholder="Model: gpt-5-nano-2025-08-07"
						aria-label="Chat model"
						autoComplete="off"
					/>
					<Input
						value={aiBaseURL}
						onChange={(event) => setAiBaseURL(event.target.value)}
						placeholder="API base URL: https://api.openai.com/v1"
						aria-label="API base URL"
						autoComplete="off"
					/>
					<Button
						type="submit"
						variant="secondary"
						disabled={aiBusy || (!aiKey.trim() && !aiModelField.trim() && !aiBaseURL.trim())}
					>
						{aiBusy ? 'Saving…' : 'Save AI settings'}
					</Button>
				</form>
				<span className={styles.hint}>
					OpenAI format: the key like <code>sk-…</code>, the model as the provider spells it, the base URL
					of any OpenAI-compatible endpoint (OpenAI, OpenRouter, a local gateway or proxy). Fill only what
					you are changing — empty fields keep their current value. Everything is stored encrypted and
					never shown again.
				</span>
				{aiNote && (
					<span className={aiNote.failed ? styles.tokenError : styles.hint}>{aiNote.text}</span>
				)}
				{aiModel != null && (
					<div className={styles.rotateRow}>
						{aiRemoveAsking ? (
							<>
								<span className={styles.hint}>Remove the AI settings saved here?</span>
								<Button variant="danger" size="sm" disabled={aiBusy} onClick={() => void removeAI()}>
									Remove
								</Button>
								<Button variant="ghost" size="sm" onClick={() => setAiRemoveAsking(false)}>
									Keep
								</Button>
							</>
						) : (
							<Button variant="secondary" size="sm" onClick={() => setAiRemoveAsking(true)}>
								Remove AI settings
							</Button>
						)}
					</div>
				)}
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
					<Button type="submit" variant="secondary" disabled={!tgToken.trim() || !tgUsername.trim() || tgBusy}>
						{tgBusy ? 'Saving…' : 'Save bot'}
					</Button>
				</form>
				<span className={styles.hint}>
					The token exactly as @BotFather printed it, and the bot's username (without @) — it makes the{' '}
					<code>t.me</code> links. Both are stored encrypted and never shown again.
				</span>
				{tgNote && <span className={tgNote.failed ? styles.tokenError : styles.hint}>{tgNote.text}</span>}
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
							smtpBusy ||
							(!smtpHost.trim() && !smtpPort.trim() && !smtpUsername.trim() && !smtpPassword.trim() && !smtpFrom.trim())
						}
					>
						{smtpBusy ? 'Saving…' : 'Save email relay'}
					</Button>
				</form>
				<span className={styles.hint}>
					Any relay works (Mailgun, SES, Postmark, your own Postfix). Fill only what you are changing —
					empty fields keep their current value. Everything is stored encrypted and never shown again.
				</span>
				{smtpNote && <span className={smtpNote.failed ? styles.tokenError : styles.hint}>{smtpNote.text}</span>}
				{/* Unconditional, unlike the AI block above (which reads its state):
				    SMTP is write-only, so a DELETE-on-nothing no-op is the honest option. */}
				<div className={styles.rotateRow}>
					{smtpRemoveAsking ? (
						<>
							<span className={styles.hint}>Remove the email relay saved here?</span>
							<Button variant="danger" size="sm" disabled={smtpBusy} onClick={() => void removeSMTP()}>
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
				open={rotateOpen}
				onClose={() => {
					setRotateOpen(false);
					setRotatedKey(null);
				}}
				title={rotatedKey ? 'Copy your new key' : 'Rotate this key?'}
			>
				{rotatedKey ? (
					<>
						{/* Not "the old key stops immediately": a rotation that breaks a
						    deployed app fails at the customer. */}
						<p className={styles.modalWarning}>
							Copy it now — this is the only time the full key is shown. The old one keeps working for{' '}
							{ROTATE_OVERLAP}, then stops.
						</p>
						<CopyField text={rotatedKey} />
						<div className={styles.modalActions}>
							<Button
								variant="primary"
								onClick={() => {
									setRotateOpen(false);
									setRotatedKey(null);
								}}
							>
								Done
							</Button>
						</div>
					</>
				) : (
					<>
						<p className={styles.modalWarning}>
							A new key is issued now. The old one keeps working for {ROTATE_OVERLAP}, so anything already
							deployed has time to pick up the new one — after that it stops.
						</p>
						<div className={styles.modalActions}>
							<Button variant="ghost" disabled={rotating} onClick={() => setRotateOpen(false)}>
								Cancel
							</Button>
							<Button
								variant="primary"
								disabled={rotating}
								onClick={async () => {
									setRotating(true);
									try {
										const res = await rotateKey();
										setRotatedKey(res.value);
										invalidateApiData('keys');
									} catch {
										// network/401 — leave the modal in its confirm state
									} finally {
										setRotating(false);
									}
								}}
							>
								{rotating ? 'Rotating…' : 'Rotate key'}
							</Button>
						</div>
					</>
				)}
			</Modal>
		</div>
	);
}
