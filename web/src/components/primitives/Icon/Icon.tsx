import type { CSSProperties } from "react";

/** Line-glyph icon set (24×24, stroke-based). */
export const ICON_PATHS = {
  overview: "M3 13h8V3H3v10zm10 8h8V11h-8v10zM3 21h8v-6H3v6zM13 3v6h8V3h-8z",
  services: "M21 16V8a2 2 0 00-1-1.73l-7-4a2 2 0 00-2 0l-7 4A2 2 0 003 8v8a2 2 0 001 1.73l7 4a2 2 0 002 0l7-4A2 2 0 0021 16zM3.27 6.96L12 12.01l8.73-5.05M12 22.08V12",
  instances: "M3 6h18M3 12h18M3 18h18",
  namespaces: "M3 7l9-4 9 4-9 4-9-4zM3 12l9 4 9-4M3 17l9 4 9-4",
  storage:
    "M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3zM4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3",
  secrets: "M6 11V8a6 6 0 0112 0v3M5 11h14v10H5zM12 15v3",
  network:
    "M12 2v6M12 16v6M2 12h6M16 12h6M7 7l3 3M17 7l-3 3M7 17l3-3M17 17l-3-3",
  logs: "M4 4h16v16H4zM8 9h8M8 13h8M8 17h5",
  identity:
    "M16 21v-2a4 4 0 00-4-4H7a4 4 0 00-4 4v2M9.5 11a4 4 0 100-8 4 4 0 000 8zM21 21v-2a4 4 0 00-3-3.87M16.5 3.13a4 4 0 010 7.75",
  search: "M11 19a8 8 0 100-16 8 8 0 000 16zM21 21l-4.3-4.3",
  chevron: "M9 6l6 6-6 6",
  chevrond: "M6 9l6 6 6-6",
  chevleft: "M15 6l-6 6 6 6",
  panel: "M4 4h16v16H4zM9 4v16",
  eye: "M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7-10-7-10-7zM12 15a3 3 0 100-6 3 3 0 000 6z",
  eyeoff: "M3 3l18 18M10.6 10.6a3 3 0 004.2 4.2M9.7 5.2A9.6 9.6 0 0112 5c6.4 0 10 7 10 7a16.7 16.7 0 01-3.4 4.1M6.3 6.3A16.6 16.6 0 002 12s3.6 7 10 7a9.5 9.5 0 003.9-.8",
  plus: "M12 5v14M5 12h14",
  bolt: "M13 2L4.5 13.5H11l-1 8.5L19.5 10H13z",
  refresh: "M21 12a9 9 0 11-2.6-6.4M21 3v6h-6",
  filter: "M4 5h16l-6 8v6l-4 2v-8z",
  close: "M6 6l12 12M18 6L6 18",
  cpu: "M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3M5 5h14v14H5zM9 9h6v6H9z",
  mem: "M3 8h18v10H3zM7 8V5M12 8V5M17 8V5M7 18v2M12 18v2M17 18v2",
  health: "M3 12h4l2 6 4-14 2 8h6",
  external: "M14 4h6v6M20 4l-9 9M19 13v6H5V5h6",
  dots: "M5 12h.01M12 12h.01M19 12h.01",
  clock: "M12 21a9 9 0 100-18 9 9 0 000 18zM12 7v5l3 2",
  scale: "M9 3H5v4M15 3h4v4M9 21H5v-4M15 21h4v-4",
  terminal: "M5 5h14v14H5zM8 9l3 3-3 3M13 15h3",
  arrowup: "M12 19V5M5 12l7-7 7 7",
  shield: "M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z",
  cube: "M12 2l9 5v10l-9 5-9-5V7zM12 12l9-5M12 12v10M12 12L3 7",
  doc: "M14 3H6v18h12V7zM14 3v4h4",
  link: "M10 13a5 5 0 007 0l3-3a5 5 0 00-7-7l-1 1M14 11a5 5 0 00-7 0l-3 3a5 5 0 007 7l1-1",
  pin: "M12 21s7-6.3 7-12a7 7 0 10-14 0c0 5.7 7 12 7 12zM12 11a2 2 0 100-4 2 2 0 000 4z",
  copy: "M10 9h9a1 1 0 011 1v9a1 1 0 01-1 1h-9a1 1 0 01-1-1v-9a1 1 0 011-1zM5 15a2 2 0 01-2-2V5a2 2 0 012-2h8a2 2 0 012 2",
  check: "M20 6L9 17l-5-5",
  alert:
    "M10.3 4.3L2.6 17.5A1.6 1.6 0 004 20h16a1.6 1.6 0 001.4-2.5L13.7 4.3a1.6 1.6 0 00-3.4 0zM12 9v5M12 17h.01",
} as const;

export type IconName = keyof typeof ICON_PATHS;

export interface IconProps {
  name: IconName;
  size?: number;
  stroke?: number;
  style?: CSSProperties;
  className?: string;
}

export function Icon({ name, size = 16, stroke = 2, style, className }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={stroke}
      strokeLinecap="round"
      strokeLinejoin="round"
      style={style}
      className={className}
      aria-hidden="true"
    >
      <path d={ICON_PATHS[name] || ICON_PATHS.dots} />
    </svg>
  );
}
