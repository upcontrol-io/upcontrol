import { useEffect, useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { Button, Input } from '@/components/primitives';
import { validEmail } from '@/lib/validEmail';
import { auth, me } from '@/lib/client';
import styles from './SignIn.module.css';

/**
 * Redeems in flight, keyed by the credentials — MODULE level on purpose: a
 * code is one-time and StrictMode mounts twice in dev, so the second effect
 * run must join the first request instead of spending the code again. Same
 * shape, same reason as the in-flight map in lib/useApiData.ts.
 */
const redeems = new Map<string, Promise<unknown>>();
function redeemOnce(email: string, token: string): Promise<unknown> {
	const key = `${email}:${token}`;
	const existing = redeems.get(key);
	if (existing) return existing;
	const request = auth.magicLink(email, token);
	redeems.set(key, request);
	return request;
}

/**
 * Two-step sign-in for a self-host: email, then the code. With SMTP the code
 * arrives by mail (and the emailed link form /signin?email=...&token=...
 * redeems on arrival); without SMTP the operator reads it from the ucapi log.
 * With UC_AUTH=none this screen is never needed: /v1/me answers for everyone,
 * and the effect below forwards straight into the app.
 */
export function SignIn() {
	const navigate = useNavigate();
	const [params] = useSearchParams();
	const linkEmail = params.get('email') ?? '';
	const linkToken = params.get('token') ?? '';
	const [email, setEmail] = useState(linkEmail);
	const [code, setCode] = useState('');
	const [error, setError] = useState('');
	const [sent, setSent] = useState(false);
	const [redeeming, setRedeeming] = useState(Boolean(linkEmail && linkToken));

	// The UC_AUTH=none path: when the session already answers, there is
	// nothing to sign in to — go to the app.
	useEffect(() => {
		let cancelled = false;
		void me()
			.then(() => {
				if (!cancelled) navigate('/', { replace: true });
			})
			.catch(() => {
				// 401 or unreachable: stay on the form.
			});
		return () => {
			cancelled = true;
		};
	}, [navigate]);

	// A link is a click already made: redeem it on arrival.
	useEffect(() => {
		if (!linkEmail || !linkToken) return;
		let cancelled = false;
		void redeemOnce(linkEmail, linkToken)
			.then(() => {
				if (!cancelled) navigate('/');
			})
			.catch(() => {
				if (cancelled) return;
				setRedeeming(false);
				setError('That link has expired or was already used. Send a new one.');
			});
		return () => {
			cancelled = true;
		};
	}, [linkEmail, linkToken, navigate]);

	async function requestCode() {
		if (!validEmail(email)) {
			setError("Enter a real email address — that's where the code goes.");
			return;
		}
		setError('');
		try {
			const resp = (await auth.magicLink(email)) as { dev_token?: string };
			// Dev mode returns the code directly; prefill it so the second step
			// is one tap.
			if (resp?.dev_token) setCode(resp.dev_token);
		} catch {
			/* non-fatal — the code may still have been issued */
		}
		setSent(true);
	}

	async function redeemCode() {
		if (!code.trim()) {
			setError('Enter the code.');
			return;
		}
		try {
			await auth.magicLink(email, code.trim());
			navigate('/');
		} catch {
			setError('That code did not work. Send a new one and try again.');
		}
	}

	return (
		<div className={styles.page}>
			<main className={styles.card}>
				<h1 className={styles.title}>
					{redeeming ? 'Signing you in' : 'Sign in to UpControl'}
				</h1>
				<p className={styles.sub}>
					{redeeming
						? 'Your link checks out. Taking you to the app.'
						: sent
							? `A one-time code was issued for ${email}. With SMTP configured it arrives by email; without it, it is in the API log.`
							: 'No password. A one-time code is issued for your email.'}
				</p>

				{redeeming ? null : !sent ? (
					<form
						className={styles.form}
						onSubmit={(event) => {
							event.preventDefault();
							void requestCode();
						}}
					>
						<Input
							label="Email"
							type="email"
							placeholder="you@company.com"
							value={email}
							error={error || undefined}
							onChange={(event) => {
								setEmail(event.target.value);
								setError('');
							}}
						/>
						<Button type="submit">Send code</Button>
					</form>
				) : (
					<form
						className={styles.form}
						onSubmit={(event) => {
							event.preventDefault();
							void redeemCode();
						}}
					>
						<Input
							label="Code"
							placeholder="the six-digit code"
							value={code}
							error={error || undefined}
							autoFocus
							onChange={(event) => {
								setCode(event.target.value);
								setError('');
							}}
						/>
						<Button type="submit">Sign in</Button>
						<button
							type="button"
							className={styles.again}
							onClick={() => {
								setSent(false);
								setCode('');
								setError('');
							}}
						>
							Use a different email
						</button>
					</form>
				)}

				<p className={styles.hint}>
					No mailer on this instance? The code is in the log:{' '}
					<code>docker compose logs ucapi | grep "sign-in code"</code>
				</p>
			</main>
		</div>
	);
}
