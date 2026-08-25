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

/** The pictogram a source card wears. The kind leads (it ties a live row to
 *  its Connect tile); unknown kinds get the puzzle piece, never a blank. */
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
