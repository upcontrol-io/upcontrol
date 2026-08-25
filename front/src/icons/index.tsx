import type { SVGProps } from 'react';

/** Shared inline SVG icon set: no icon font, no icon library. */
type IconProps = SVGProps<SVGSVGElement>;

function base(children: React.ReactNode, props: IconProps, viewBox = '0 0 32 32') {
  return (
    <svg width={14} height={14} viewBox={viewBox} fill="none" aria-hidden="true" {...props}>
      {children}
    </svg>
  );
}

export function CopyIcon(props: IconProps) {
  return base(
    <>
      <path d="M21 21H27V5H11V11" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" />
      <path d="M21 11H5V27H21V11Z" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" />
    </>,
    props,
  );
}

export function CheckIcon(props: IconProps) {
  return base(<path d="M5 18L12 25L28 9" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" />, props);
}

export function ChevronIcon(props: IconProps) {
  return base(
    <path d="M9 12L16 19L23 12" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" />,
    props,
  );
}

/** A gear: hub + rim + eight teeth. Was a bare circle while nothing used it. */
export function SettingsIcon(props: IconProps) {
  return base(
    <>
      <circle cx={16} cy={16} r={7.5} stroke="currentColor" strokeWidth={2} />
      <circle cx={16} cy={16} r={2.75} stroke="currentColor" strokeWidth={2} />
      <path
        d="M16 5v3.5M16 23.5V27M5 16h3.5M23.5 16H27M8.2 8.2l2.5 2.5M21.3 21.3l2.5 2.5M23.8 8.2l-2.5 2.5M10.7 21.3l-2.5 2.5"
        stroke="currentColor"
        strokeWidth={2}
        strokeLinecap="round"
      />
    </>,
    props,
  );
}

/** An (i) in a ring — the "where is this explained" affordance. */
export function InfoIcon(props: IconProps) {
  return base(
    <>
      <circle cx={16} cy={16} r={12} stroke="currentColor" strokeWidth={2} />
      <path d="M16 15v7" stroke="currentColor" strokeWidth={2.5} strokeLinecap="round" />
      <circle cx={16} cy={10.25} r={1.75} fill="currentColor" />
    </>,
    props,
  );
}

export function MoonIcon(props: IconProps) {
  return base(
    <path
      d="M27 18.5A11 11 0 1 1 13.5 5a9 9 0 0 0 13.5 13.5Z"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
  );
}

export function SunIcon(props: IconProps) {
  return base(
    <>
      <circle cx={16} cy={16} r={6} stroke="currentColor" strokeWidth={2} />
      <path
        d="M16 3V6M16 26V29M29 16H26M6 16H3M25 7L23 9M9 23L7 25M25 25L23 23M9 9L7 7"
        stroke="currentColor"
        strokeWidth={2}
        strokeLinecap="round"
      />
    </>,
    props,
  );
}

export function DeployIcon(props: IconProps) {
  return base(
    <path d="M16 25V8M16 8L9 15M16 8L23 15" stroke="currentColor" strokeWidth={2.5} strokeLinecap="round" strokeLinejoin="round" />,
    props,
  );
}

export function ErrorIcon(props: IconProps) {
  return base(
    <path d="M16 6L28 26H4L16 6M16 13V18" stroke="currentColor" strokeWidth={2.5} strokeLinecap="round" strokeLinejoin="round" />,
    props,
  );
}

export function PaymentIcon(props: IconProps) {
  return base(
    <>
      <rect x={4} y={9} width={24} height={16} rx={2} stroke="currentColor" strokeWidth={2.5} />
      <path d="M4 14H28" stroke="currentColor" strokeWidth={2.5} />
    </>,
    props,
  );
}

export function CheckEventIcon(props: IconProps) {
  return base(
    <path d="M6 17L13 24L26 8" stroke="currentColor" strokeWidth={2.5} strokeLinecap="round" strokeLinejoin="round" />,
    props,
  );
}

export function CloseIcon(props: IconProps) {
  return base(
    <path d="M8 8L24 24M24 8L8 24" stroke="currentColor" strokeWidth={2} strokeLinecap="round" />,
    props,
  );
}

/** Activity polyline — the product's pulse identity (HealthLine, PulseLine). */
export function PulseIcon(props: IconProps) {
  return base(
    <path
      d="M3 17H9L13 8L19 25L23 17H29"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
  );
}

export function MoreIcon(props: IconProps) {
  return base(
    <>
      <circle cx={7} cy={16} r={2.25} fill="currentColor" />
      <circle cx={16} cy={16} r={2.25} fill="currentColor" />
      <circle cx={25} cy={16} r={2.25} fill="currentColor" />
    </>,
    props,
  );
}


/** Site checks — the probe watching a URL from outside. */
export function GlobeIcon(props: IconProps) {
  return base(
    <path
      d="M2 12H22M2 12C2 17.5228 6.47715 22 12 22M2 12C2 6.47715 6.47715 2 12 2M22 12C22 17.5228 17.5228 22 12 22M22 12C22 6.47715 17.5228 2 12 2M12 2C14.5013 4.73835 15.9228 8.29203 16 12C15.9228 15.708 14.5013 19.2616 12 22M12 2C9.49872 4.73835 8.07725 8.29203 8 12C8.07725 15.708 9.49872 19.2616 12 22"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** App logs — the console prompt, which is what a log stream looks like. */
export function TerminalIcon(props: IconProps) {
  return base(
    <path
      d="M4 17L10 11L4 5M12 19H20"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** Cron / heartbeat sources — a job that reports on schedule. */
export function ClockIcon(props: IconProps) {
  return base(
    <path
      d="M12 6V12L16 14M22 12C22 17.5228 17.5228 22 12 22C6.47715 22 2 17.5228 2 12C2 6.47715 6.47715 2 12 2C17.5228 2 22 6.47715 22 12Z"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** Deploy hooks — a launch, not our up-arrow DeployIcon (that one marks timeline events). */
export function RocketIcon(props: IconProps) {
  return base(
    <path
      d="M12 14.9998L9 11.9998M12 14.9998C13.3968 14.4685 14.7369 13.7985 16 12.9998M12 14.9998V19.9998C12 19.9998 15.03 19.4498 16 17.9998C17.08 16.3798 16 12.9998 16 12.9998M9 11.9998C9.53214 10.6192 10.2022 9.29582 11 8.04976C12.1652 6.18675 13.7876 4.65281 15.713 3.59385C17.6384 2.53489 19.8027 1.98613 22 1.99976C22 4.71976 21.22 9.49976 16 12.9998M9 11.9998H4C4 11.9998 4.55 8.96976 6 7.99976C7.62 6.91976 11 7.99976 11 7.99976M4.5 16.4998C3 17.7598 2.5 21.4998 2.5 21.4998C2.5 21.4998 6.24 20.9998 7.5 19.4998C8.21 18.6598 8.2 17.3698 7.41 16.5898C7.02131 16.2188 6.50929 16.0044 5.97223 15.9878C5.43516 15.9712 4.91088 16.1535 4.5 16.4998Z"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** GitHub source — the branch glyph, the one git shape everyone knows. */
export function GitBranchIcon(props: IconProps) {
  return base(
    <path
      d="M3 3V13.2C3 14.8802 3 15.7202 3.32698 16.362C3.6146 16.9265 4.07354 17.3854 4.63803 17.673C5.27976 18 6.11984 18 7.8 18H15M15 18C15 19.6569 16.3431 21 18 21C19.6569 21 21 19.6569 21 18C21 16.3431 19.6569 15 18 15C16.3431 15 15 16.3431 15 18ZM3 8L15 8M15 8C15 9.65686 16.3431 11 18 11C19.6569 11 21 9.65685 21 8C21 6.34315 19.6569 5 18 5C16.3431 5 15 6.34315 15 8Z"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** Fallback for a source kind nothing here names yet — an integration piece. */
export function PuzzleIcon(props: IconProps) {
  return base(
    <path
      d="M7.5 4.5C7.5 3.11929 8.61929 2 10 2C11.3807 2 12.5 3.11929 12.5 4.5V6H13.5C14.8978 6 15.5967 6 16.1481 6.22836C16.8831 6.53284 17.4672 7.11687 17.7716 7.85195C18 8.40326 18 9.10218 18 10.5H19.5C20.8807 10.5 22 11.6193 22 13C22 14.3807 20.8807 15.5 19.5 15.5H18V17.2C18 18.8802 18 19.7202 17.673 20.362C17.3854 20.9265 16.9265 21.3854 16.362 21.673C15.7202 22 14.8802 22 13.2 22H12.5V20.25C12.5 19.0074 11.4926 18 10.25 18C9.00736 18 8 19.0074 8 20.25V22H6.8C5.11984 22 4.27976 22 3.63803 21.673C3.07354 21.3854 2.6146 20.9265 2.32698 20.362C2 19.7202 2 18.8802 2 17.2V15.5H3.5C4.88071 15.5 6 14.3807 6 13C6 11.6193 4.88071 10.5 3.5 10.5H2C2 9.10218 2 8.40326 2.22836 7.85195C2.53284 7.11687 3.11687 6.53284 3.85195 6.22836C4.40326 6 5.10218 6 6.5 6H7.5V4.5Z"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** Telegram channel — the paper plane its own logo is. */
export function SendIcon(props: IconProps) {
  return base(
    <path
      d="M10.4995 13.5001L20.9995 3.00005M10.6271 13.8281L13.2552 20.5861C13.4867 21.1815 13.6025 21.4791 13.7693 21.566C13.9139 21.6414 14.0862 21.6415 14.2308 21.5663C14.3977 21.4796 14.5139 21.1821 14.7461 20.587L21.3364 3.69925C21.5461 3.16207 21.6509 2.89348 21.5935 2.72185C21.5437 2.5728 21.4268 2.45583 21.2777 2.40604C21.1061 2.34871 20.8375 2.45352 20.3003 2.66315L3.41258 9.25349C2.8175 9.48572 2.51997 9.60183 2.43326 9.76873C2.35809 9.91342 2.35819 10.0857 2.43353 10.2303C2.52043 10.3971 2.81811 10.5128 3.41345 10.7444L10.1715 13.3725C10.2923 13.4195 10.3527 13.443 10.4036 13.4793C10.4487 13.5114 10.4881 13.5509 10.5203 13.596C10.5566 13.6468 10.5801 13.7073 10.6271 13.8281Z"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}
