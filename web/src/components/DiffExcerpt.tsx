import { classifyDiffLine } from "../logic";

export function DiffExcerpt({ text }: { text: string }) {
  const lines = text.split("\n");
  return (
    <pre className="diff">
      {lines.map((l, i) => (
        <div key={i} className={`dl dl-${classifyDiffLine(l)}`}>
          {l || " "}
        </div>
      ))}
    </pre>
  );
}
