/** Estado de prontidão do processo. O /readyz o consulta; o desligamento o desliga primeiro. */
export interface Lifecycle {
  isReady(): boolean;
  markShuttingDown(): void;
}

export function createLifecycle(): Lifecycle {
  let ready = true;
  return {
    isReady: () => ready,
    markShuttingDown: () => {
      ready = false;
    },
  };
}
