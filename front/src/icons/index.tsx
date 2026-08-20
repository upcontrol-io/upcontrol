import type { SVGProps } from 'react';

/** Shared inline SVG icon set (design-brief §Assets — no icon font, no icon library). */
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

/* --- Mobile bottom tab bar (BottomTabBar) --------------------------------- */

export function HomeIcon(props: IconProps) {
  return base(
    <path
      d="M5 14L16 5L27 14M8 12V26H24V12"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
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

/** Alerts tab — the one section that is about being told something. */
export function BellIcon(props: IconProps) {
  return base(
    <path
      d="M8 13a8 8 0 1 1 16 0c0 6 2 8 2 8H6s2-2 2-8M13 26a3 3 0 0 0 6 0"
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

/* --- Source & channel pictograms (docs/icons, Untitled UI line set) -------
   These replaced the three-letter chips on source cards and channel rows
   (user decision, Aug 14, 2026): "DEP" or "ML" had to be decoded, a rocket
   or an envelope does not. All are 24-grid, stroke 2, achromatic. */

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

/** Email channel — an envelope. */
export function MailIcon(props: IconProps) {
  return base(
    <path
      d="M2 7L10.1649 12.7154C10.8261 13.1783 11.1567 13.4097 11.5163 13.4993C11.8339 13.5785 12.1661 13.5785 12.4837 13.4993C12.8433 13.4097 13.1739 13.1783 13.8351 12.7154L22 7M6.8 20H17.2C18.8802 20 19.7202 20 20.362 19.673C20.9265 19.3854 21.3854 18.9265 21.673 18.362C22 17.7202 22 16.8802 22 15.2V8.8C22 7.11984 22 6.27976 21.673 5.63803C21.3854 5.07354 20.9265 4.6146 20.362 4.32698C19.7202 4 18.8802 4 17.2 4H6.8C5.11984 4 4.27976 4 3.63803 4.32698C3.07354 4.6146 2.6146 5.07354 2.32698 5.63803C2 6.27976 2 7.11984 2 8.8V15.2C2 16.8802 2 17.7202 2.32698 18.362C2.6146 18.9265 3.07354 19.3854 3.63803 19.673C4.27976 20 5.11984 20 6.8 20Z"
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

/** Discord channel — chat bubbles; no brand marks in the set, and the brief
    keeps other companies' logos out of our achromatic UI anyway. */
export function MessageChatIcon(props: IconProps) {
  return base(
    <path
      d="M6.09436 11.2288C6.03221 10.8282 5.99996 10.4179 5.99996 10C5.99996 5.58172 9.60525 2 14.0526 2C18.4999 2 22.1052 5.58172 22.1052 10C22.1052 10.9981 21.9213 11.9535 21.5852 12.8345C21.5154 13.0175 21.4804 13.109 21.4646 13.1804C21.4489 13.2512 21.4428 13.301 21.4411 13.3735C21.4394 13.4466 21.4493 13.5272 21.4692 13.6883L21.8717 16.9585C21.9153 17.3125 21.9371 17.4895 21.8782 17.6182C21.8266 17.731 21.735 17.8205 21.6211 17.8695C21.4911 17.9254 21.3146 17.8995 20.9617 17.8478L17.7765 17.3809C17.6101 17.3565 17.527 17.3443 17.4512 17.3448C17.3763 17.3452 17.3245 17.3507 17.2511 17.3661C17.177 17.3817 17.0823 17.4172 16.893 17.4881C16.0097 17.819 15.0524 18 14.0526 18C13.6344 18 13.2237 17.9683 12.8227 17.9073M7.63158 22C10.5965 22 13 19.5376 13 16.5C13 13.4624 10.5965 11 7.63158 11C4.66668 11 2.26316 13.4624 2.26316 16.5C2.26316 17.1106 2.36028 17.6979 2.53955 18.2467C2.61533 18.4787 2.65322 18.5947 2.66566 18.6739C2.67864 18.7567 2.68091 18.8031 2.67608 18.8867C2.67145 18.9668 2.65141 19.0573 2.61134 19.2383L2 22L4.9948 21.591C5.15827 21.5687 5.24 21.5575 5.31137 21.558C5.38652 21.5585 5.42641 21.5626 5.50011 21.5773C5.5701 21.5912 5.67416 21.6279 5.88227 21.7014C6.43059 21.8949 7.01911 22 7.63158 22Z"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** Slack channel — the hash every Slack channel name starts with. */
export function HashIcon(props: IconProps) {
  return base(
    <path
      d="M9.49999 3L6.49999 21M17.5 3L14.5 21M20.5 8H3.5M19.5 16H2.5"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** Webhook channel — the bolt the whole automation world uses for "fires on an event". */
export function ZapIcon(props: IconProps) {
  return base(
    <path
      d="M13 2L4.09344 12.6879C3.74463 13.1064 3.57023 13.3157 3.56756 13.4925C3.56524 13.6461 3.63372 13.7923 3.75324 13.8889C3.89073 14 4.16316 14 4.70802 14H12L11 22L19.9065 11.3121C20.2553 10.8936 20.4297 10.6843 20.4324 10.5075C20.4347 10.3539 20.3663 10.2077 20.2467 10.1111C20.1092 10 19.8368 10 19.292 10H12L13 2Z"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
    />,
    props,
    '0 0 24 24',
  );
}

/** The one full-color icon in the set: Google's brand mark for auth buttons
    (brief's achromatic rule covers our UI, not another company's logo). */
/** The real OpenAI blossom (docs/icons/openai.svg, 24×24), labelling Codex in
    the hero's agent row. fill is currentColor — the source ships black, which
    would vanish on the night hero, so the caller sets a light color. The other
    agent logos there are raster tiles under src/assets/agents/. */
export function OpenAIIcon(props: IconProps) {
  return base(
    <path
      fill="currentColor"
      d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"
    />,
    props,
    '0 0 24 24',
  );
}
