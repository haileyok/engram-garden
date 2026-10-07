// A sprout: the app's mark.
export function Logo({ size = 18 }: { size?: number }) {
  return (
    <svg className="sprout" width={size} height={size} viewBox="0 0 24 24" aria-hidden fill="currentColor">
      <path d="M11 21v-7.2C7.2 13.6 4 11 4 6.5V5h1.5C9 5 11.3 7.2 11.8 10.2 12.6 7 15.3 4 19 4h1v1.5c0 4.6-3.3 7.6-7 7.9V21h-2z" />
    </svg>
  );
}
