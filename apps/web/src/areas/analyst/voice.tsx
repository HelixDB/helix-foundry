// Hands-free voice with GPT-Live, inside the ordinary chat. One session per
// workspace lives here, above the routes, so a conversation started on Home
// continues into the chat its first question opens. The browser carries the
// microphone and speaker over WebRTC; the API creates the session and answers
// GPT-Live's delegations with ordinary Analyst runs (apps/api/src/live.ts).
import "./voice.css";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api";
import { cx } from "../../kit";
import { chatListKey } from "./launcher";
import { analystPaths } from "./paths";

export type VoiceStatus = "off" | "connecting" | "live";
export type Voice = {
  // GPT-Live is usable with this workspace's AI provider.
  available: boolean;
  status: VoiceStatus;
  reconnecting: boolean;
  // The chat this session belongs to; null until Home's first question.
  chatId: string | null;
  // The user's latest words and the assistant's, for display only.
  heard: string;
  caption: string;
  // A delegated request is being worked out.
  working: boolean;
  // The microphone is paused while the user types.
  paused: boolean;
  error: string | null;
  toggle: (chatId: string | null) => void;
  stop: () => void;
  pauseForTyping: () => void;
  resumeListening: () => void;
  // A question typed (not spoken) while voice is on.
  noteTyped: (goal: string) => void;
};
const off: Voice = {
  available: false,
  status: "off",
  reconnecting: false,
  chatId: null,
  heard: "",
  caption: "",
  working: false,
  paused: false,
  error: null,
  toggle: () => {},
  stop: () => {},
  pauseForTyping: () => {},
  resumeListening: () => {},
  noteTyped: () => {},
};
const VoiceContext = createContext<Voice>(off);
export const useVoice = () => useContext(VoiceContext);

type Claim = {
  status: string;
  action: "ask" | "clarify" | "end" | "none" | null;
  runId: string | null;
  chatId: string | null;
  error: string | null;
};
// What a failure to start means, in words the user can act on.
function startError(e: unknown) {
  const name = (e as { name?: string })?.name;
  if (name === "NotAllowedError" || name === "SecurityError")
    return "Microphone access was blocked. Allow it in the browser’s site settings, or keep typing.";
  if (name === "NotFoundError" || name === "OverconstrainedError")
    return "No microphone was found. Connect one, or keep typing.";
  if (name === "NotReadableError")
    return "The microphone is in use by another app. Close it, or keep typing.";
  return (e as Error)?.message || "Voice couldn’t start. You can keep typing.";
}
const closedText: Record<string, string> = {
  expired:
    "The voice session reached its time limit. Start voice again to continue.",
  content:
    "The voice session was ended by a safety filter. You can keep typing.",
  connection_lost:
    "The voice connection was lost. Start voice again, or keep typing.",
};

// One GPT-Live connection: microphone, speaker and event channel.
class Connection {
  peer = new RTCPeerConnection();
  events: RTCDataChannel;
  audio = new Audio();
  microphone?: MediaStream;
  sessionId?: string;
  started = false;
  finished = false;
  private timers: ReturnType<typeof setTimeout>[] = [];
  constructor(
    private on: {
      event: (event: any) => void;
      lost: () => void;
      blockedAudio: () => void;
    },
  ) {
    this.audio.autoplay = true;
    this.audio.setAttribute("playsinline", "");
    this.peer.addEventListener("track", (e) => {
      this.audio.srcObject = new MediaStream([e.track]);
      this.audio.play().catch(() => this.on.blockedAudio());
    });
    // Created before the offer, as the connection requires.
    this.events = this.peer.createDataChannel("oai-events");
    this.events.addEventListener("message", ({ data }) => {
      let event: any;
      try {
        event = JSON.parse(data);
      } catch {
        return;
      }
      if (event.type === "session.started") this.started = true;
      if (event.type === "session.closed") this.finished = true;
      this.on.event(event);
    });
    this.events.addEventListener("close", () => {
      if (!this.finished) this.on.lost();
    });
    this.peer.addEventListener("connectionstatechange", () => {
      const state = this.peer.connectionState;
      if (state === "failed") this.on.lost();
      // A brief network blip can recover by itself.
      if (state === "disconnected")
        this.later(() => {
          if (this.peer.connectionState === "disconnected") this.on.lost();
        }, 4000);
    });
  }
  later(fn: () => void, ms: number) {
    this.timers.push(setTimeout(fn, ms));
  }
  async open(workspaceId: string, chatId: string | null) {
    this.microphone = await navigator.mediaDevices.getUserMedia({
      // Echo cancellation keeps the assistant's voice out of the microphone.
      audio: {
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
    });
    if (this.finished) {
      // Stopped while the permission prompt was open.
      this.microphone.getTracks().forEach((t) => t.stop());
      throw new Error("Voice was stopped");
    }
    for (const track of this.microphone.getAudioTracks())
      this.peer.addTrack(track, this.microphone);
    await this.peer.setLocalDescription(await this.peer.createOffer());
    await this.gathered();
    const sdp = this.peer.localDescription?.sdp;
    if (!sdp) throw new Error("Voice couldn’t start: no connection offer.");
    const session = await api<{
      sessionId: string;
      sdp: string;
      chatId: string | null;
    }>(`/workspaces/${workspaceId}/live/sessions`, { sdp, chatId });
    this.sessionId = session.sessionId;
    if (this.finished) throw new Error("Voice was stopped");
    await this.peer.setRemoteDescription({ type: "answer", sdp: session.sdp });
    return session;
  }
  private gathered() {
    if (this.peer.iceGatheringState === "complete") return Promise.resolve();
    return new Promise<void>((resolve, reject) => {
      const done = () => {
        if (this.peer.iceGatheringState !== "complete") return;
        clearTimeout(timer);
        this.peer.removeEventListener("icegatheringstatechange", done);
        resolve();
      };
      const timer = setTimeout(() => {
        this.peer.removeEventListener("icegatheringstatechange", done);
        reject(
          new Error("Voice couldn’t connect: the network didn’t respond."),
        );
      }, 10_000);
      this.peer.addEventListener("icegatheringstatechange", done);
    });
  }
  send(command: Record<string, unknown>) {
    if (this.events.readyState === "open")
      this.events.send(JSON.stringify(command));
  }
  setListening(on: boolean) {
    for (const t of this.microphone?.getAudioTracks() || []) t.enabled = on;
  }
  // Stops capture and playback at once; the session itself finishes with
  // session.closed, or is dropped after a few seconds.
  close(graceful: boolean) {
    this.microphone?.getTracks().forEach((t) => t.stop());
    this.audio.muted = true;
    const release = () => {
      this.timers.forEach(clearTimeout);
      this.finished = true;
      this.events.close();
      this.peer.close();
      this.audio.srcObject = null;
    };
    if (graceful && this.started && !this.finished) {
      this.send({ type: "session.close" });
      const check = setInterval(() => {
        if (this.finished) {
          clearInterval(check);
          release();
        }
      }, 100);
      this.later(() => {
        clearInterval(check);
        release();
      }, 4000);
    } else release();
  }
}

export function VoiceProvider({
  workspaceId,
  children,
}: {
  workspaceId: string;
  children: ReactNode;
}) {
  const qc = useQueryClient(),
    navigate = useNavigate(),
    { pathname } = useLocation();
  const availability = useQuery({
    queryKey: [workspaceId, "live"],
    queryFn: () =>
      api<{ available: boolean; reason?: string }>(
        `/workspaces/${workspaceId}/live`,
      ),
    staleTime: 60_000,
    retry: false,
  });
  const supported =
    typeof window !== "undefined" &&
    "RTCPeerConnection" in window &&
    !!navigator.mediaDevices?.getUserMedia;
  const available = supported && !!availability.data?.available;

  const [status, setStatus] = useState<VoiceStatus>("off"),
    [reconnecting, setReconnecting] = useState(false),
    [chatId, setChatId] = useState<string | null>(null),
    [heard, setHeard] = useState(""),
    [caption, setCaption] = useState(""),
    [working, setWorking] = useState(0),
    [paused, setPaused] = useState(false),
    [error, setError] = useState<string | null>(null),
    [announce, setAnnounce] = useState("");
  const conn = useRef<Connection | null>(null),
    chat = useRef<string | null>(null),
    // The display groups words into turns; it never decides what runs.
    turn = useRef({ userFresh: true, assistantFresh: true }),
    retries = useRef(0),
    // A question was just typed on Home: its chat becomes this session's.
    adopt = useRef(false),
    quiet = useRef<ReturnType<typeof setTimeout>>(undefined),
    path = useRef(pathname);
  path.current = pathname;

  const bind = useCallback(
    (id: string) => {
      chat.current = id;
      setChatId(id);
      if (path.current !== analystPaths.thread(id))
        navigate(analystPaths.thread(id));
    },
    [navigate],
  );

  const end = useCallback(
    (message: string | null, graceful = true) => {
      const c = conn.current;
      conn.current = null;
      if (c) {
        c.close(graceful);
        if (c.sessionId)
          void fetch(
            `/api/v1/workspaces/${workspaceId}/live/sessions/${encodeURIComponent(c.sessionId)}`,
            { method: "DELETE", credentials: "same-origin", keepalive: true },
          ).catch(() => {});
      }
      setStatus("off");
      setReconnecting(false);
      setHeard("");
      setCaption("");
      setWorking(0);
      setPaused(false);
      setError(message);
      adopt.current = false;
      if (c) setAnnounce(message || "Voice off. You can keep typing.");
    },
    [workspaceId],
  );

  const delegated = useCallback(
    async (c: Connection, delegationId: string) => {
      setWorking((n) => n + 1);
      turn.current.userFresh = true;
      try {
        const claim = await api<Claim>(
          `/workspaces/${workspaceId}/live/sessions/${encodeURIComponent(c.sessionId!)}/delegations/${encodeURIComponent(delegationId)}`,
          {},
        );
        if (conn.current !== c) return;
        if (claim.runId) {
          // The question now shows in the chat; the composer is free again.
          setHeard("");
          void qc.invalidateQueries({ queryKey: chatListKey(workspaceId) });
        }
        if (claim.chatId && claim.chatId !== chat.current) bind(claim.chatId);
        if (claim.action === "end")
          c.later(() => conn.current === c && end(null), 2500);
      } catch {
        // GPT-Live hears about failures from the server; the chat still works.
      } finally {
        setWorking((n) => Math.max(0, n - 1));
      }
    },
    [workspaceId, qc, bind, end],
  );

  const connect = useCallback(
    async (target: string | null) => {
      const c = new Connection({
        event: (event) => {
          if (conn.current !== c) return;
          switch (event.type) {
            case "session.started":
              retries.current = 0;
              setStatus("live");
              setReconnecting(false);
              setAnnounce("Voice on. Listening.");
              return;
            case "session.input_transcript.delta":
              // A new utterance: the assistant's last words are done.
              if (turn.current.userFresh) {
                setHeard("");
                setCaption("");
              }
              turn.current.userFresh = false;
              turn.current.assistantFresh = true;
              setHeard((h) => (h + event.delta).slice(-600));
              return;
            case "session.output_transcript.delta":
              if (turn.current.assistantFresh) setCaption("");
              turn.current.assistantFresh = false;
              turn.current.userFresh = true;
              setCaption((t) => t + event.delta);
              // Its words fade from the hint once it has stopped speaking.
              clearTimeout(quiet.current);
              quiet.current = setTimeout(() => setCaption(""), 6000);
              return;
            case "session.delegation.created":
              if (event.delegation?.id) void delegated(c, event.delegation.id);
              return;
            case "session.closed":
              end(
                event.reason && event.reason !== "close_requested"
                  ? closedText[event.reason] || closedText.connection_lost
                  : null,
                false,
              );
              return;
          }
        },
        lost: () => {
          if (conn.current !== c) return;
          // One quiet reconnect into the same chat; the server restores its
          // history and does not rerun what already ran.
          if (c.started && retries.current < 1) {
            retries.current++;
            c.close(false);
            conn.current = null;
            setReconnecting(true);
            setStatus("connecting");
            void connect(chat.current);
          } else end(closedText.connection_lost, false);
        },
        blockedAudio: () =>
          setError(
            "The browser blocked the assistant’s audio. Click the page to hear it.",
          ),
      });
      conn.current = c;
      c.later(() => {
        if (conn.current === c && !c.started)
          end("Voice took too long to connect. Try again, or keep typing.");
      }, 20_000);
      try {
        const session = await c.open(workspaceId, target);
        if (conn.current !== c) return;
        if (session.chatId) {
          chat.current = session.chatId;
          setChatId(session.chatId);
        }
      } catch (e) {
        if (conn.current === c) end(startError(e), false);
      }
    },
    [workspaceId, delegated, end],
  );

  const start = useCallback(
    (target: string | null) => {
      if (!available || conn.current) return;
      chat.current = target;
      setChatId(target);
      setError(null);
      setHeard("");
      setCaption("");
      setPaused(false);
      retries.current = 0;
      turn.current = { userFresh: true, assistantFresh: true };
      setStatus("connecting");
      setAnnounce("Connecting voice…");
      void connect(target);
    },
    [available, connect],
  );

  // Leaving the conversation ends voice: it never follows the user elsewhere.
  useEffect(() => {
    if (!conn.current) return;
    const id = chat.current;
    if (id ? pathname === analystPaths.thread(id) : pathname === "/") return;
    const opened = /^\/analyst\/([^/]+)$/.exec(pathname);
    if (!id && opened && adopt.current) {
      adopt.current = false;
      const typedChat = decodeURIComponent(opened[1]);
      chat.current = typedChat;
      setChatId(typedChat);
      const sid = conn.current.sessionId;
      if (sid)
        void api(
          `/workspaces/${workspaceId}/live/sessions/${encodeURIComponent(sid)}/context`,
          { chatId: typedChat },
        ).catch(() => {});
      return;
    }
    end(null);
  }, [pathname, workspaceId, end]);
  // A provider change (or lost GPT-Live access) ends voice.
  useEffect(() => {
    if (!available && conn.current)
      end("Voice ended: it needs the OpenAI provider with GPT-Live.");
  }, [available, end]);
  // Escape stops voice, unless a dialog or text composition is using it.
  useEffect(() => {
    if (status === "off") return;
    const key = (e: KeyboardEvent) => {
      if (
        e.key !== "Escape" ||
        e.defaultPrevented ||
        e.isComposing ||
        document.querySelector("[role=dialog], [role=alertdialog]")
      )
        return;
      e.preventDefault();
      end(null);
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [status, end]);
  // A blocked autoplay recovers on the next click.
  useEffect(() => {
    if (!error?.includes("blocked the assistant’s audio")) return;
    const resume = () => {
      void conn.current?.audio.play().then(() => setError(null));
    };
    window.addEventListener("pointerdown", resume, { once: true });
    return () => window.removeEventListener("pointerdown", resume);
  }, [error]);
  // Switching workspace or closing the app releases everything.
  useEffect(() => () => conn.current?.close(true), []);

  const value = useMemo<Voice>(
    () => ({
      available,
      status,
      reconnecting,
      chatId,
      heard,
      caption,
      working: working > 0,
      paused,
      error,
      toggle: (target) => {
        if (conn.current) end(null);
        else start(target);
      },
      stop: () => end(null),
      pauseForTyping: () => {
        if (!conn.current || paused) return;
        conn.current.setListening(false);
        setPaused(true);
        setHeard("");
      },
      resumeListening: () => {
        if (!conn.current || !paused) return;
        conn.current.setListening(true);
        turn.current.userFresh = true;
        setPaused(false);
      },
      noteTyped: (goal) => {
        const c = conn.current;
        if (!c?.sessionId) return;
        if (!chat.current) adopt.current = true;
        void api(
          `/workspaces/${workspaceId}/live/sessions/${encodeURIComponent(c.sessionId)}/context`,
          { typed: goal },
        ).catch(() => {});
      },
    }),
    [
      available,
      status,
      reconnecting,
      chatId,
      heard,
      caption,
      working,
      paused,
      error,
      start,
      end,
      workspaceId,
    ],
  );
  return (
    <VoiceContext.Provider value={value}>
      {children}
      <span className="hf-sr-only" aria-live="polite">
        {announce}
      </span>
    </VoiceContext.Provider>
  );
}

// The orb: starts and stops voice. It glows while a session is live.
export function VoiceOrb({
  chatId,
  placement,
}: {
  chatId: string | null;
  placement: "home" | "chat";
}) {
  const voice = useVoice();
  if (!voice.available && voice.status === "off") return null;
  const label =
    voice.status === "live"
      ? "Stop voice conversation"
      : voice.status === "connecting"
        ? "Cancel voice connection"
        : "Start voice conversation";
  return (
    <button
      type="button"
      className={cx("hf-voice-orb", `is-${placement}`, `is-${voice.status}`)}
      aria-pressed={voice.status === "live"}
      aria-label={label}
      title={voice.status === "off" ? "Talk with ChatGPT voice" : label}
      onClick={() => voice.toggle(chatId)}
    >
      <span className="hf-voice-orb-mark" aria-hidden />
    </button>
  );
}

// The line under a composer while voice is on: real state only.
export function voiceHint(voice: Voice): string | null {
  if (voice.error) return voice.error;
  if (voice.status === "connecting")
    return voice.reconnecting ? "Reconnecting voice…" : "Connecting voice…";
  if (voice.status !== "live") return null;
  if (voice.paused)
    return "Voice paused while you type · send or clear the text to resume";
  if (voice.working) return "Checking your data…";
  const said = voice.caption.trim();
  if (said) return said.length > 160 ? "…" + said.slice(-160) : said;
  return "Listening · speak naturally · Esc to stop";
}
