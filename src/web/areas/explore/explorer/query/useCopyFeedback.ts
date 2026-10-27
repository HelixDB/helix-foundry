import { useCallback, useEffect, useState } from "react";

/** Copies text and remembers what was copied for 1.5s, for a "Copied" state. */
export function useCopyFeedback(): {
  copied: string | null;
  copy: (label: string, text: string) => void;
} {
  const [copied, setCopied] = useState<string | null>(null);

  useEffect(() => {
    if (!copied) return;
    const timer = window.setTimeout(() => setCopied(null), 1_500);
    return () => window.clearTimeout(timer);
  }, [copied]);

  const copy = useCallback((label: string, text: string) => {
    void navigator.clipboard
      ?.writeText(text)
      .then(() => setCopied(label))
      .catch(() => setCopied(null));
  }, []);

  return { copied, copy };
}
