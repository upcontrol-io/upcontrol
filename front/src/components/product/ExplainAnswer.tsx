import type { ReactNode } from 'react';
import { Badge, Tooltip, type BadgeTone } from '@/components/primitives';
import { CodeBlock, CopyButton, CopyField } from '@/components/code';
import type { ExplainResult } from '@/lib/client';
import styles from './ExplainAnswer.module.css';

export interface ExplainAnswerProps {
	/** The AI triage answer, as POST /v1/logs/explain returns it. */
	result: ExplainResult;
	/** The exact wire bytes that were sent to the model — the selection the
	 *  answer was read from, so the copy pair can carry the evidence along. */
	lines?: readonly string[];
	className?: string;
}

/**
 * One scanner for every literal a triage answer quotes back, in priority
 * order: backtick spans first (their fences are unambiguous), then quoted
 * spans, then the token shapes. Alternation order is the priority — the
 * engine takes the earliest-listed alternative at each position, and
 * matchAll resumes past a whole match, so a fenced span chips as one literal
 * instead of being re-scanned for its parts.
 */
const PROSE_LITERAL = new RegExp(
	[
		'`[^`\\n]{1,60}`',
		'"[^"\\n]{1,58}"',
		"'[^'\\n]{1,58}'",
		'\\b\\d{2}:\\d{2}(?::\\d{2})?\\b',
		// Trailing guard is (?!\\w), not \\b: \\b after '%' never holds (both
		// sides non-word), yet "85%" is a literal the answer must chip. For the
		// letter units the two are the same assertion.
		'\\b\\d+(?:\\.\\d+)?(?:ms|s|m|h|%)(?!\\w)',
		'\\b\\d{3,}\\b',
		'\\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\\b',
		'/[\\w.-]+(?:/[\\w.-]+)+',
	].join('|'),
	'g',
);

/**
 * Prose with its literals chipped: matched spans render as mono inline code,
 * everything between them stays verbatim plain text (the paragraphs are
 * white-space: pre-wrap — nothing is trimmed, ever). Deliberately
 * conservative: "5 steps" is not a unit, an apostrophe never opens a span,
 * and a quote whose neighbours break the boundary guards is left plain
 * rather than guessed at. The clipboard builders below are NOT this — what a
 * person pastes stays plain text.
 */
function renderProse(text: string): ReactNode[] {
	const parts: ReactNode[] = [];
	let cursor = 0;
	let chips = 0;
	for (const match of text.matchAll(PROSE_LITERAL)) {
		const start = match.index ?? 0;
		const literal = match[0];
		let chip: string | null = null;
		if (literal.startsWith('`')) {
			chip = literal.slice(1, -1);
		} else if (literal.startsWith('"') || literal.startsWith("'")) {
			// A quote is a literal only at word boundaries: opened at string
			// start or after breathing space, closed at the end or before
			// punctuation. This is what keeps "checkout's" from ever opening
			// a span.
			const before = start > 0 ? text[start - 1] : '';
			const after = text[start + literal.length] ?? '';
			if ((before === '' || /[\s(]/.test(before)) && (after === '' || /[\s),.;:]/.test(after))) {
				chip = literal;
			}
		} else {
			chip = literal;
		}
		if (chip === null) continue;
		if (start > cursor) parts.push(text.slice(cursor, start));
		parts.push(
			<code className={styles.mono} key={chips++}>
				{chip}
			</code>,
		);
		cursor = start + literal.length;
	}
	if (cursor < text.length) parts.push(text.slice(cursor));
	return parts;
}

/** Confidence grade to badge tone — the shape carries the grade; the string-
 *  keyed read is what degrades an unexpected wire value to neutral. */
const CONFIDENCE_TONES: Record<ExplainResult['confidence'], BadgeTone> = {
	high: 'ok',
	medium: 'check',
	low: 'neutral',
};

function confidenceTone(confidence: string): BadgeTone {
	return (CONFIDENCE_TONES as Record<string, BadgeTone | undefined>)[confidence] ?? 'neutral';
}

/**
 * The answer as plain text, for a ticket or a chat. The grade rides in the
 * text ("medium confidence"), not just the badge above it — plain text has no
 * colours, so the guess stays labelled wherever it is pasted.
 */
function explainAnswerText(result: ExplainResult): string {
	const lines = [
		result.problem,
		`Likely cause (${result.confidence} confidence): ${result.cause}`,
	];
	if (result.fix) lines.push(`Fix: ${result.fix}`);
	const steps = result.investigate ?? [];
	if (steps.length > 0) {
		lines.push('Investigate:');
		steps.forEach((step, i) => {
			lines.push(`${i + 1}. ${step.step}`);
			if (step.command) lines.push(`    ${step.command}`);
		});
	}
	return lines.join('\n');
}

/**
 * The same answer plus the wire bytes the model read, for pasting into
 * another model. The [Context]/[Explanation] boundary is the point: the
 * receiving model must see where the observed lines end and the inferred
 * answer begins.
 */
function explainWithContext(result: ExplainResult, lines: readonly string[]): string {
	return `[Context]:\n${lines.join('\n')}\n\n[Explanation]:\n${explainAnswerText(result)}`;
}

/**
 * The one renderer for an AI explain answer — the logs panel and the incident
 * card ask the same question, so the answer reads the same everywhere.
 *
 * Triage law (CLAUDE.md): the problem states what the lines show as fact, the
 * cause is labelled a guess with its grade in text (Badge is a shape, the
 * grade rides inside it — never colour alone), the fix is a suggestion, and
 * every investigate step carries a runnable command in the copy grammar the
 * rest of the product already uses (CopyField for one line, CodeBlock for
 * more). The footer is a sentence, not a bar: explain quota is a count beside
 * the answer, not a track to fill.
 */
export function ExplainAnswer({ result, lines, className }: ExplainAnswerProps) {
	const steps = result.investigate ?? [];
	return (
		<div className={[styles.answer, className].filter(Boolean).join(' ')}>
			<div className={styles.block}>
				<span className={styles.label}>What the lines show</span>
				<p className={styles.text}>{renderProse(result.problem)}</p>
			</div>
			<div className={styles.block}>
				<span className={styles.label}>
					Likely cause
					<Badge tone={confidenceTone(result.confidence)}>{result.confidence} confidence</Badge>
				</span>
				<p className={styles.text}>{renderProse(result.cause)}</p>
			</div>
			{result.fix && (
				<div className={styles.block}>
					<span className={styles.label}>Suggested fix</span>
					<p className={styles.text}>{renderProse(result.fix)}</p>
				</div>
			)}
			{steps.length > 0 && (
				<div className={styles.block}>
					<span className={styles.label}>
						Investigate<span className={styles.labelNote}>{steps.length} steps, in order</span>
					</span>
					{/* The ol announces the order; the printed number is presentation,
					    so it carries aria-hidden and is not read twice. */}
					<ol className={styles.steps}>
						{steps.map((step, i) => (
							<li key={i} className={styles.step}>
								<span className={styles.stepNum} aria-hidden="true">
									{i + 1}
								</span>
								<div className={styles.stepBody}>
									<span className={styles.stepText}>{renderProse(step.step)}</span>
									{step.command &&
										(step.command.includes('\n') ? (
											<CodeBlock
												code={step.command}
												language="cURL"
												showLineNumbers={false}
												className={styles.commandBlock}
											/>
										) : (
											<CopyField text={step.command} className={styles.commandField} />
										))}
								</div>
							</li>
						))}
					</ol>
				</div>
			)}
			{/* What a person forwards — to a ticket, a chat, or another model that
			    has to check the evidence. Same pair, same labels as the incident
			    card's triage row, through the same CopyButton: every copy
			    affordance in the product goes through that one component. The
			    pair has hierarchy: the answer alone is the primary action, the
			    context variant steps back until hovered. The context variant
			    renders only when the caller had the wire bytes — it is the heavy
			    one, and most of the time the answer alone is the message. */}
			<div className={styles.actions}>
				<Tooltip
					content="The answer on its own — the problem, the labelled guess and the steps, for a ticket or a chat."
					interactiveChild
				>
					<CopyButton
						text={explainAnswerText(result)}
						label="Copy response"
						className={styles.actionPrimary}
					/>
				</Tooltip>
				{lines && lines.length > 0 && (
					<Tooltip
						content={`The same answer plus the ${lines.length} log lines it was read from — for pasting into another model that has to check the evidence.`}
						interactiveChild
					>
						<CopyButton
							text={explainWithContext(result, lines)}
							label="Copy with context"
							className={styles.actionSecondary}
						/>
					</Tooltip>
				)}
			</div>
			<p className={styles.foot}>
				{result.cached
					? 'cached read · nothing spent'
					: `AI explain · ${result.limit > 0 ? `${result.used} of ${result.limit} used` : `${result.used} used`} this month`}
			</p>
		</div>
	);
}
