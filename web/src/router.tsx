// A small history-based router: the app has only a few pages.

import { createContext, useContext, useEffect, useState, type MouseEvent, type ReactNode } from "react";

type Location = { path: string; query: URLSearchParams };

const RouterContext = createContext<{ loc: Location; navigate: (to: string) => void }>({
  loc: { path: "/", query: new URLSearchParams() },
  navigate: () => {},
});

function current(): Location {
  return { path: window.location.pathname, query: new URLSearchParams(window.location.search) };
}

export function Router({ children }: { children: ReactNode }) {
  const [loc, setLoc] = useState<Location>(current);
  useEffect(() => {
    const onPop = () => setLoc(current());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);
  const navigate = (to: string) => {
    if (to === window.location.pathname + window.location.search) return;
    window.history.pushState(null, "", to);
    setLoc(current());
    window.scrollTo(0, 0);
  };
  return <RouterContext.Provider value={{ loc, navigate }}>{children}</RouterContext.Provider>;
}

export function useRouter() {
  return useContext(RouterContext);
}

export function Link({ to, children, className }: { to: string; children: ReactNode; className?: string }) {
  const { navigate } = useRouter();
  const onClick = (e: MouseEvent<HTMLAnchorElement>) => {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to);
  };
  return (
    <a href={to} onClick={onClick} className={className}>
      {children}
    </a>
  );
}
