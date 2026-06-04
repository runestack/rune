import {
  Badge, Button, Card, CardHead, Dot, Dropdown, Icon, ICON_PATHS, Logo, PageHead,
  Replicas, Search, Segmented, Spark, Tabs, Tag, UsageBar,
  TweaksPanel, TweakSection, TweakRadio, TweakColor,
} from "../components";
import type { IconName } from "../components";
import { useTweaks } from "../lib/theme";
import { useState } from "react";
import "./Playground.css";

function Spec({ name, note, children }: { name: string; note?: string; children: React.ReactNode }) {
  return (
    <div className="spec">
      <div className="spec-name">
        {name}
        {note && <small>{note}</small>}
      </div>
      <div className="spec-demo">{children}</div>
    </div>
  );
}

export function Playground() {
  const [t, setTweak] = useTweaks();
  const [seg, setSeg] = useState("all");
  const [tab, setTab] = useState("overview");

  return (
    <div className="pg">
      <div className="pg-wrap">
        <div className="eyebrow">Rune · Component Library</div>
        <h1 className="pg-title">
          Component <em>playground</em>
        </h1>
        <p className="pg-lede">
          Every shared primitive, in isolation. Recolor and reshape the whole system live from the Tweaks panel
          — tokens flow through CSS variables, so accent and edges restyle everything at once.
        </p>

        <section className="pg-sect">
          <h2>Brand</h2>
          <Spec name="Logo" note="4 variants">
            <Logo variant="wordmark" /> <Logo variant="mark" /> <Logo variant="tile" /> <Logo variant="mono" />
          </Spec>
        </section>

        <section className="pg-sect">
          <h2>Status</h2>
          <Spec name="Dot" note="color = attention">
            {(["run", "deploy", "warn", "fail", "idle", "net"] as const).map((s) => (
              <Dot key={s} s={s} />
            ))}
            <Dot s="run" pulse />
          </Spec>
          <Spec name="Badge" note="Running stays quiet">
            <Badge s="run" /> <Badge s="deploy" /> <Badge s="warn" /> <Badge s="fail" /> <Badge s="idle" />
            <Badge s="accent">RBAC</Badge>
          </Spec>
          <Spec name="Replicas">
            <Replicas ready={3} want={3} /> <Replicas ready={1} want={2} />
          </Spec>
          <Spec name="UsageBar" note="amber > 75, red > 88">
            <UsageBar v={34} /> <UsageBar v={78} /> <UsageBar v={92} />
          </Spec>
          <Spec name="Spark">
            <Spark data={[38, 41, 44, 40, 52, 58, 55, 61, 57, 63, 68, 71, 66, 72]} />
          </Spec>
          <Spec name="Tag">
            <Tag>env:prod</Tag> <Tag>10.244.0.11</Tag> <Tag>RWO</Tag>
          </Spec>
        </section>

        <section className="pg-sect">
          <h2>Buttons</h2>
          <Spec name="Button" note="variants">
            <Button variant="primary">
              <Icon name="plus" /> Cast secret
            </Button>
            <Button>Restart</Button>
            <Button variant="ghost">Cancel</Button>
            <Button size="sm">Small</Button>
            <Button icon>
              <Icon name="dots" />
            </Button>
            <Button disabled>Disabled</Button>
          </Spec>
        </section>

        <section className="pg-sect">
          <h2>Inputs</h2>
          <Spec name="Search">
            <Search />
          </Spec>
          <Spec name="Segmented" note="finite sets only">
            <Segmented
              options={["all", "running", "degraded", "failed"]}
              value={seg}
              onChange={setSeg}
            />
          </Spec>
          <Spec name="Dropdown" note="unbounded → searchable">
            <Dropdown
              label={
                <span className="dd-lab">
                  <span className="eyebrow">NS</span>
                  <b>production</b>
                </span>
              }
            >
              {(close) => (
                <div className="dd-list">
                  {["production", "staging", "observability", "ingress-system", "default"].map((n) => (
                    <div key={n} className="dd-item" onClick={close}>
                      <Dot s="run" />
                      {n}
                    </div>
                  ))}
                </div>
              )}
            </Dropdown>
          </Spec>
          <Spec name="Tabs">
            <Tabs
              tabs={[
                { id: "overview", label: "Overview" },
                { id: "instances", label: "Instances" },
                { id: "logs", label: "Logs" },
              ]}
              active={tab}
              onChange={setTab}
            />
          </Spec>
        </section>

        <section className="pg-sect">
          <h2>Layout</h2>
          <Spec name="PageHead">
            <div style={{ width: "100%" }}>
              <PageHead eyebrow="Cluster" title="Cluster <em>overview</em>" sub="Everything running across rune-prod, at a glance." />
            </div>
          </Spec>
          <Spec name="Card">
            <Card style={{ width: 320 }}>
              <CardHead actions={<Button size="sm" variant="ghost"><Icon name="refresh" /></Button>}>Services at a glance</CardHead>
              <div className="card-pad" style={{ color: "var(--text-2)", fontSize: 13 }}>
                12 services · 11 running · 1 degraded
              </div>
            </Card>
          </Spec>
        </section>

        <section className="pg-sect">
          <h2>Icons</h2>
          <Spec name="Icon" note={`${Object.keys(ICON_PATHS).length} glyphs`}>
            {(Object.keys(ICON_PATHS) as IconName[]).map((n) => (
              <span key={n} title={n} style={{ color: "var(--text-2)" }}>
                <Icon name={n} size={18} />
              </span>
            ))}
          </Spec>
        </section>
      </div>

      <TweaksPanel>
        <TweakSection label="Brand" />
        <TweakRadio label="Logo" value={t.logo} options={["mark", "tile", "wordmark", "mono"]} onChange={(v) => setTweak("logo", v)} />
        <TweakColor
          label="Accent"
          value={t.accent}
          options={["#9e8cfc", "#30a46c", "#67ddfd", "#f76809", "#d6409f", "#daa16e"]}
          onChange={(v) => setTweak("accent", v)}
        />
        <TweakSection label="Surface" />
        <TweakRadio label="Edges" value={t.edges} options={["soft", "crisp", "sharp"]} onChange={(v) => setTweak("edges", v)} />
      </TweaksPanel>
    </div>
  );
}
