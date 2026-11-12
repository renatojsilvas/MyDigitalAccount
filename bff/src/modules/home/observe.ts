import { trace, type Context, type Meter } from "@opentelemetry/api";
import type { DegradedReason } from "./schema.js";

/** Seções opcionais da home que podem faltar. Conjunto fixo: é rótulo de métrica. */
export type HomeSection = "statement";

/** Porta de observabilidade do serviço da home: o serviço não conhece OpenTelemetry. */
export interface HomeObserver {
  degraded(section: HomeSection, reason: DegradedReason, context: Context): void;
}

export function createHomeObserver(meter: Meter): HomeObserver {
  const degraded = meter.createCounter("bff.home.degraded", {
    description: "Respostas da home com uma seção opcional indisponível",
  });
  return {
    degraded(section, reason, context) {
      degraded.add(1, { "home.section": section, "home.degraded.reason": reason }, context);
      // No span do servidor (pai do contexto): é o que liga a métrica ao trace.
      const span = trace.getSpan(context);
      span?.setAttribute("home.degraded", true);
      span?.addEvent("home.degraded", { "home.section": section, "home.degraded.reason": reason });
    },
  };
}
