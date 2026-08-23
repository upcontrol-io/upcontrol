import { Link } from 'react-router-dom';
import type { CSSProperties } from 'react';
import { BrandMark } from '@/components/primitives';
import styles from './Wordmark.module.css';

export interface WordmarkProps {
	/** The header's own logo class — supplies size and colour, which differ per screen. */
	className?: string;
	size?: number;
}

type Px = [x: number, y: number];

// The kit's own 5-wide bitmap alphabet, local pixels per letter, copied
// verbatim. Cap-height letters (U, C, t, l) run y:0-6; x-height letters
// (o, n, r) run y:2-6; p keeps the x-height top but descends to y:8.
const GLYPHS: Record<string, Px[]> = {
	U: [[0,0],[0,1],[0,2],[0,3],[0,4],[0,5],[4,0],[4,1],[4,2],[4,3],[4,4],[4,5],[1,6],[2,6],[3,6]],
	p: [[0,2],[0,3],[0,4],[0,5],[0,6],[0,7],[0,8],[1,2],[2,2],[3,2],[4,3],[4,4],[1,5],[2,5],[3,5]],
	C: [[1,0],[2,0],[3,0],[0,1],[4,1],[0,2],[0,3],[0,4],[0,5],[4,5],[1,6],[2,6],[3,6]],
	o: [[1,2],[2,2],[3,2],[0,3],[4,3],[0,4],[4,4],[0,5],[4,5],[1,6],[2,6],[3,6]],
	n: [[0,2],[0,3],[0,4],[0,5],[0,6],[1,2],[2,2],[3,2],[4,3],[4,4],[4,5],[4,6]],
	t: [[1,0],[1,1],[1,2],[1,3],[1,4],[1,5],[0,2],[2,2],[1,6],[2,6]],
	r: [[0,2],[0,3],[0,4],[0,5],[0,6],[1,2],[2,2]],
	l: [[0,0],[0,1],[0,2],[0,3],[0,4],[0,5],[0,6],[1,6]],
};

// "UpControl", the kit's own spelling and word table, letter for letter and
// cell for cell.
const WORD: Array<[letter: string, x: number]> = [
	['U', 0], ['p', 6], ['C', 12], ['o', 18], ['n', 24], ['t', 30], ['r', 34], ['o', 38], ['l', 44],
];

const WORD_WIDTH = 46; // cells — the kit's own lockup math ends the word here
const CARET_TOP = 1;
const CARET_HEIGHT = 7; // the kit's own caret rect, independent of any one letter's x-height or descender

// The kit's wordmark-only viewBox: 1 cell of padding above/left, 2 below
// (clearing p's descender).
const VB_X = -1;
const VB_Y = -1;
const VB_W = 48;
const VB_H = 11;

/**
 * Mark + name, the way every header in the product shows the brand. Both
 * halves are the kit's own pixel language: the mark is BrandMark's 16x16
 * grid, the word is its 5-wide bitmap alphabet, so the lockup reads as one
 * drawn object rather than an icon beside a web-font label.
 *
 * The word plays the kit's 3a Typewriter on hover only: at rest every letter
 * is visible; hovering restarts the reveal on the kit's own 0.117s-per-letter
 * step grid behind a travelling var(--ok) caret, which parks and blinks once
 * the word is complete. Un-hovering removes every animation, so the word is
 * instantly whole again — this is a decoration on the resting logotype, not
 * a loading state. The mechanics (letters, carets) live in
 * Wordmark.module.css.
 */
// One step up from the mark's own size — the word reads a size larger than
// the icon beside it, which is the visual balance every header carries.
const WORD_SIZE_STEP = 2;

export function Wordmark({ className, size = 18 }: WordmarkProps) {
	const wordSize = size + WORD_SIZE_STEP;
	const wordWidth = (wordSize * VB_W) / VB_H;

	return (
		<Link to="/" className={[styles.wordmark, className].filter(Boolean).join(' ')} aria-label="UpControl, home">
			<BrandMark size={size} className={styles.mark} />
			<svg
				className={styles.word}
				width={wordWidth}
				height={wordSize}
				viewBox={`${VB_X} ${VB_Y} ${VB_W} ${VB_H}`}
				aria-hidden="true"
			>
				{WORD.map(([letter, x], i) => (
					<g key={`${letter}${x}`} className={styles.letter} style={{ '--i': i } as CSSProperties} fill="currentColor">
						{GLYPHS[letter].map(([px, py]) => (
							<rect key={`${px}:${py}`} x={x + px} y={py} width={1} height={1} />
						))}
					</g>
				))}
				{WORD.map(([letter, x], i) => (
					<rect
						key={`caret-${letter}${x}`}
						className={styles.caret}
						style={{ '--i': i } as CSSProperties}
						x={x}
						y={CARET_TOP}
						width={1}
						height={CARET_HEIGHT}
					/>
				))}
				<rect className={styles.caretEnd} x={WORD_WIDTH} y={CARET_TOP} width={1} height={CARET_HEIGHT} />
			</svg>
		</Link>
	);
}
