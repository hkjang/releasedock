import { useCallback, useEffect, useRef, useState, type Dispatch, type SetStateAction } from 'react';

export interface AsyncState<T> {
  data?: T;
  error?: Error;
  loading: boolean;
  reload: () => Promise<void>;
  setData: Dispatch<SetStateAction<T | undefined>>;
}

export function useAsync<T>(loader: () => Promise<T>, dependencies: readonly unknown[] = []): AsyncState<T> {
  const loaderRef = useRef(loader);
  loaderRef.current = loader;
  const [data, setData] = useState<T>();
  const [error, setError] = useState<Error>();
  const [loading, setLoading] = useState(true);
  const requestSequence = useRef(0);

  const reload = useCallback(async () => {
    const sequence = ++requestSequence.current;
    setLoading(true);
    setError(undefined);
    try {
      const result = await loaderRef.current();
      if (sequence === requestSequence.current) setData(result);
    } catch (reason) {
      if (sequence === requestSequence.current) setError(reason instanceof Error ? reason : new Error('알 수 없는 오류가 발생했습니다.'));
    } finally {
      if (sequence === requestSequence.current) setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, dependencies);

  useEffect(() => {
    void reload();
  }, [reload]);

  return { data, error, loading, reload, setData };
}
