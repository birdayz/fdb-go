import { useState } from 'react';

/** Local text for an input that commits on blur; reset whenever the committed value changes. */
export function useDraft(value: string): [string, (s: string) => void] {
  const [draft, setDraft] = useState(value);
  const [committed, setCommitted] = useState(value);
  if (value !== committed) {
    setCommitted(value);
    setDraft(value);
  }
  return [draft, setDraft];
}
