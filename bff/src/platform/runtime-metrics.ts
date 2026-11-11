import { monitorEventLoopDelay } from "node:perf_hooks";
import type { Meter } from "@opentelemetry/api";

/**
 * Métricas de recurso (método USE) do processo Node: atraso do event loop e heap.
 * São a base do load shedding da fase 6 (@fastify/under-pressure).
 * Devolve a função que para a coleta.
 */
export function registerRuntimeMetrics(meter: Meter): () => void {
  const loopDelay = monitorEventLoopDelay({ resolution: 20 });
  loopDelay.enable();

  meter
    .createObservableGauge("nodejs.eventloop.delay.p99", {
      unit: "s",
      description: "Percentil 99 do atraso do event loop desde a última coleta",
    })
    .addCallback((result) => {
      result.observe(loopDelay.percentile(99) / 1e9);
      loopDelay.reset();
    });

  meter
    .createObservableGauge("nodejs.heap.used", {
      unit: "By",
      description: "Heap do V8 em uso",
    })
    .addCallback((result) => {
      result.observe(process.memoryUsage().heapUsed);
    });

  return () => {
    loopDelay.disable();
  };
}
