import type { SVGProps } from 'react';
import {
  ClockIcon,
  GitBranchIcon,
  GlobeIcon,
  PaymentIcon,
  PuzzleIcon,
  RocketIcon,
  TerminalIcon,
} from '@/icons';

/**
 * The pictogram a source card wears, resolved from what the row actually is.
 * It replaced the three-letter chip ("URL", "DEP", "CRN" — user decision,
 * Aug 14, 2026): an abbreviation has to be decoded, the icon does not.
 *
 * The kind leads (it is what ties a live row to its Connect tile); the mark is
 * the fallback for rows that carry none — the two derived rows and the mock's
 * cron. A kind nothing here names yet gets the puzzle piece, never a blank.
 */
type IconComponent = (props: SVGProps<SVGSVGElement>) => React.ReactNode;

const BY_KIND: Record<string, IconComponent> = {
  deployhooks: RocketIcon,
  vercel: RocketIcon,
  github: GitBranchIcon,
  stripe: PaymentIcon,
  payments: PaymentIcon,
};

const BY_MARK: Record<string, IconComponent> = {
  URL: GlobeIcon,
  LOG: TerminalIcon,
  CRN: ClockIcon,
  DEP: RocketIcon,
  GIT: GitBranchIcon,
  PAY: PaymentIcon,
  STR: PaymentIcon,
};

export function SourceIcon({
  source,
  ...props
}: { source: { kind?: string; mark: string } } & SVGProps<SVGSVGElement>) {
  const Icon =
    (source.kind && BY_KIND[source.kind]) || BY_MARK[source.mark] || PuzzleIcon;
  return <Icon {...props} />;
}
