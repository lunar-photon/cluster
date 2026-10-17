#include <ftxui/component/component.hpp>
#include <ftxui/component/event.hpp>
#include <ftxui/component/screen_interactive.hpp>
#include <ftxui/dom/elements.hpp>
#include <ftxui/dom/table.hpp>

#include <atomic>
#include <chrono>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <map>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

namespace fs = std::filesystem;
using namespace ftxui;

struct Config {
    std::string host = "";
    std::string port = "8888";
    std::string slurm_partition = "medium";
    std::string slurm_account = "";
    std::string slurm_qos = "";
    std::string slurm_cpus = "4";
    std::string slurm_mem = "8G";
    std::string slurm_time = "04:00:00";
    std::string gpu_partition = "gpu";
    std::string gpu_qos = "";
    std::string mount_point = "";
    std::string notebook_dir = "";
};

std::string get_home_dir() {
    const char* home = std::getenv("HOME");
    return home ? std::string(home) : "/tmp";
}

fs::path get_config_path() {
    const char* xdg = std::getenv("XDG_CONFIG_HOME");
    fs::path base = xdg ? fs::path(xdg) : (fs::path(get_home_dir()) / ".config");
    return base / "cluster" / "config";
}

fs::path get_legacy_config_path() {
    const char* xdg = std::getenv("XDG_CONFIG_HOME");
    fs::path base = xdg ? fs::path(xdg) : (fs::path(get_home_dir()) / ".config");
    return base / "cluster-jupyter" / "config";
}

Config load_config() {
    Config cfg;
    cfg.mount_point = get_home_dir() + "/Cluster";

    fs::path p = get_config_path();
    if (!fs::exists(p)) {
        p = get_legacy_config_path();
    }

    if (fs::exists(p)) {
        std::ifstream f(p);
        std::string line;
        while (std::getline(f, line)) {
            if (line.empty() || line[0] == '#') continue;
            auto eq = line.find('=');
            if (eq != std::string::npos) {
                std::string k = line.substr(0, eq);
                std::string v = line.substr(eq + 1);
                // trim whitespace and quotes
                while (!v.empty() && (v.front() == '"' || v.front() == '\'')) v.erase(v.begin());
                while (!v.empty() && (v.back() == '"' || v.back() == '\'')) v.pop_back();

                // replace $HOME
                auto home_pos = v.find("$HOME");
                if (home_pos != std::string::npos) {
                    v.replace(home_pos, 5, get_home_dir());
                }

                if (k == "HOST") cfg.host = v;
                else if (k == "PORT") cfg.port = v;
                else if (k == "SLURM_PARTITION") cfg.slurm_partition = v;
                else if (k == "SLURM_ACCOUNT") cfg.slurm_account = v;
                else if (k == "SLURM_QOS") cfg.slurm_qos = v;
                else if (k == "SLURM_CPUS") cfg.slurm_cpus = v;
                else if (k == "SLURM_MEM") cfg.slurm_mem = v;
                else if (k == "SLURM_TIME") cfg.slurm_time = v;
                else if (k == "GPU_PARTITION") cfg.gpu_partition = v;
                else if (k == "GPU_QOS") cfg.gpu_qos = v;
                else if (k == "MOUNT_POINT") cfg.mount_point = v;
                else if (k == "NOTEBOOK_DIR") cfg.notebook_dir = v;
            }
        }
    }
    return cfg;
}

void save_config(const Config& cfg) {
    fs::path p = get_config_path();
    fs::create_directories(p.parent_path());
    std::ofstream f(p);
    f << "# cluster configuration file\n";
    f << "# Generated via cluster C++ TUI\n\n";
    f << "HOST=\"" << cfg.host << "\"\n";
    f << "PORT=" << cfg.port << "\n";
    f << "SLURM_PARTITION=\"" << cfg.slurm_partition << "\"\n";
    f << "SLURM_ACCOUNT=\"" << cfg.slurm_account << "\"\n";
    f << "SLURM_QOS=\"" << cfg.slurm_qos << "\"\n";
    f << "SLURM_CPUS=" << cfg.slurm_cpus << "\"\n";
    f << "SLURM_MEM=\"" << cfg.slurm_mem << "\"\n";
    f << "SLURM_TIME=\"" << cfg.slurm_time << "\"\n";
    f << "GPU_PARTITION=\"" << cfg.gpu_partition << "\"\n";
    f << "GPU_QOS=\"" << cfg.gpu_qos << "\"\n";
    f << "MOUNT_POINT=\"" << cfg.mount_point << "\"\n";
    f << "NOTEBOOK_DIR=\"" << cfg.notebook_dir << "\"\n";
}

bool check_is_mounted(const std::string& mount_path) {
    std::ifstream mounts("/proc/mounts");
    std::string line;
    std::string norm_path = fs::weakly_canonical(mount_path).string();
    while (std::getline(mounts, line)) {
        std::istringstream iss(line);
        std::string dev, target;
        if (iss >> dev >> target) {
            if (target == norm_path || target == mount_path) {
                return true;
            }
        }
    }
    return false;
}

std::vector<std::string> run_pipe_cmd(const std::string& cmd) {
    std::vector<std::string> output;
    FILE* fp = popen(cmd.c_str(), "r");
    if (!fp) return output;
    char buf[1024];
    while (fgets(buf, sizeof(buf), fp)) {
        std::string line(buf);
        while (!line.empty() && (line.back() == '\n' || line.back() == '\r')) line.pop_back();
        if (!line.empty()) output.push_back(line);
    }
    pclose(fp);
    return output;
}

void parse_delimited(const std::string& line, char delim, std::vector<std::string>& out) {
    out.clear();
    std::stringstream ss(line);
    std::string item;
    while (std::getline(ss, item, delim)) {
        out.push_back(item);
    }
}

int main() {
    auto screen = ScreenInteractive::Fullscreen();
    Config cfg = load_config();

    // State
    int selected_tab = 0;
    std::vector<std::string> tab_titles = {
        "  Workspace & Actions  ",
        "  Cluster Monitor  ",
        "  Settings  "
    };

    std::string feedback_msg = "";
    std::vector<std::vector<std::string>> jobs_data = {
        {"JOBID", "PARTITION", "NAME", "USER", "STATE", "TIME", "NODES", "NODELIST"}
    };
    std::vector<std::vector<std::string>> nodes_data = {
        {"PARTITION", "AVAIL", "TIMELIMIT", "NODES", "STATE", "NODELIST"}
    };

    auto refresh_monitor = [&]() {
        if (cfg.host.empty()) return;
        std::string q_cmd = "ssh -o BatchMode=yes -o ConnectTimeout=3 " + cfg.host + " 'squeue -u $USER -o \"%i|%P|%j|%u|%T|%M|%D|%R\"' 2>/dev/null";
        auto lines = run_pipe_cmd(q_cmd);
        jobs_data.clear();
        jobs_data.push_back({"JOBID", "PARTITION", "NAME", "USER", "STATE", "TIME", "NODES", "NODELIST"});
        for (size_t i = 1; i < lines.size(); ++i) {
            std::vector<std::string> row;
            parse_delimited(lines[i], '|', row);
            if (row.size() >= 8) {
                jobs_data.push_back({row[0], row[1], row[2], row[3], row[4], row[5], row[6], row[7]});
            }
        }

        std::string i_cmd = "ssh -o BatchMode=yes -o ConnectTimeout=3 " + cfg.host + " 'sinfo -o \"%P|%a|%l|%D|%T|%N\"' 2>/dev/null";
        auto n_lines = run_pipe_cmd(i_cmd);
        nodes_data.clear();
        nodes_data.push_back({"PARTITION", "AVAIL", "TIMELIMIT", "NODES", "STATE", "NODELIST"});
        for (size_t i = 1; i < n_lines.size(); ++i) {
            std::vector<std::string> row;
            parse_delimited(n_lines[i], '|', row);
            if (row.size() >= 6) {
                nodes_data.push_back({row[0], row[1], row[2], row[3], row[4], row[5]});
            }
        }
    };

    std::atomic<bool> is_refreshing{false};
    auto trigger_refresh = [&]() {
        if (is_refreshing) return;
        is_refreshing = true;
        feedback_msg = "Refreshing status...";
        screen.PostEvent(Event::Custom);
        std::thread([&] {
            refresh_monitor();
            is_refreshing = false;
            feedback_msg = "";
            screen.PostEvent(Event::Custom);
        }).detach();
    };

    // Run external command with terminal I/O restored
    auto exec_command = [&](const std::string& cmd) {
        screen.WithRestoredIO([&] {
            std::system(cmd.c_str());
        })();
        trigger_refresh();
    };

    // Tab 1: Actions Buttons
    auto btn_dev_cpu = Button("Dev Workspace (CPU)", [&] { exec_command("cluster dev -b"); });
    auto btn_dev_gpu = Button("Dev Workspace (GPU)", [&] { exec_command("cluster dev --gpu -b"); });

    auto btn_shell_cpu = Button("Interactive CPU Shell", [&] { exec_command("cluster shell"); });
    auto btn_shell_gpu = Button("Interactive GPU Shell", [&] { exec_command("cluster shell --gpu"); });

    auto btn_jup_head  = Button("JupyterLab (Head Node)", [&] { exec_command("cluster jupyter"); });
    auto btn_jup_slurm = Button("JupyterLab (Slurm CPU)", [&] { exec_command("cluster jupyter --slurm"); });
    auto btn_jup_gpu   = Button("JupyterLab (GPU Node)",   [&] { exec_command("cluster jupyter --gpu"); });

    auto btn_mount   = Button("Mount Cluster Drive",   [&] { exec_command("cluster mount -b"); });
    auto btn_unmount = Button("Unmount Cluster Drive", [&] { exec_command("cluster unmount"); });

    auto btn_sync_push = Button("Push Code -> Cluster", [&] { exec_command("cluster sync push"); });
    auto btn_sync_pull = Button("Pull Code <- Cluster", [&] { exec_command("cluster sync pull"); });

    auto btn_quit = Button("Quit [q]", [&] { screen.Exit(); });

    auto actions_container = Container::Vertical({
        Container::Horizontal({btn_dev_cpu, btn_dev_gpu}),
        Container::Horizontal({btn_shell_cpu, btn_shell_gpu}),
        Container::Horizontal({btn_jup_head, btn_jup_slurm, btn_jup_gpu}),
        Container::Horizontal({btn_mount, btn_unmount}),
        Container::Horizontal({btn_sync_push, btn_sync_pull}),
        Container::Horizontal({btn_quit})
    });

    // Tab 2: Monitor
    auto btn_refresh = Button("Refresh Status", [&] { trigger_refresh(); });
    auto monitor_container = Container::Vertical({
        btn_refresh
    });

    // Tab 3: Settings Inputs
    std::string in_host = cfg.host;
    std::string in_port = cfg.port;
    std::string in_part = cfg.slurm_partition;
    std::string in_account = cfg.slurm_account;
    std::string in_qos = cfg.slurm_qos;
    std::string in_cpus = cfg.slurm_cpus;
    std::string in_mem = cfg.slurm_mem;
    std::string in_gpu_part = cfg.gpu_partition;
    std::string in_gpu_qos = cfg.gpu_qos;
    std::string in_mount = cfg.mount_point;

    auto comp_host = Input(&in_host, "Cluster Hostname or alias");
    auto comp_part = Input(&in_part, "Default Slurm partition");
    auto comp_account = Input(&in_account, "Slurm Account");
    auto comp_qos = Input(&in_qos, "Slurm QOS");
    auto comp_cpus = Input(&in_cpus, "CPU Cores");
    auto comp_mem = Input(&in_mem, "Memory allocation");
    auto comp_gpu_part = Input(&in_gpu_part, "GPU Partition");
    auto comp_gpu_qos = Input(&in_gpu_qos, "GPU QOS");
    auto comp_mount = Input(&in_mount, "Local mount path");

    auto btn_save_cfg = Button("Save Configuration", [&] {
        cfg.host = in_host;
        cfg.slurm_partition = in_part;
        cfg.slurm_account = in_account;
        cfg.slurm_qos = in_qos;
        cfg.slurm_cpus = in_cpus;
        cfg.slurm_mem = in_mem;
        cfg.gpu_partition = in_gpu_part;
        cfg.gpu_qos = in_gpu_qos;
        cfg.mount_point = in_mount;
        save_config(cfg);
        feedback_msg = "Configuration saved successfully!";
    });

    auto settings_container = Container::Vertical({
        comp_host,
        comp_part,
        comp_account,
        comp_qos,
        comp_cpus,
        comp_mem,
        comp_gpu_part,
        comp_gpu_qos,
        comp_mount,
        btn_save_cfg
    });

    // Tab Selector
    auto tab_toggle = Toggle(&tab_titles, &selected_tab);
    auto tab_container = Container::Tab({actions_container, monitor_container, settings_container}, &selected_tab);

    auto main_container = Container::Vertical({
        tab_toggle,
        tab_container
    });

    // Catch keyboard shortcuts
    auto main_with_events = CatchEvent(main_container, [&](Event event) {
        if (event == Event::Character('q') || event == Event::Character('Q') || event == Event::Escape) {
            screen.Exit();
            return true;
        }
        if (event == Event::Character('r')) {
            trigger_refresh();
            return true;
        }
        if (event == Event::Character('d')) {
            exec_command("cluster dev -b");
            return true;
        }
        if (event == Event::Character('j')) {
            exec_command("cluster jupyter");
            return true;
        }
        if (event == Event::Character('m')) {
            if (check_is_mounted(cfg.mount_point)) {
                exec_command("cluster unmount");
            } else {
                exec_command("cluster mount -b");
            }
            return true;
        }
        return false;
    });

    // Renderer
    auto renderer = Renderer(main_with_events, [&] {
        bool mounted = check_is_mounted(cfg.mount_point);
        std::string mount_str = mounted ? ("Mounted at " + cfg.mount_point) : "Unmounted";
        Color mount_col = mounted ? Color::Green : Color::GrayDark;

        // Top Header
        auto header = hbox({
            text(" cluster ") | bold | bgcolor(Color::Cyan) | color(Color::Black),
            text(" HPC Developer Suite (C++) ") | bold | color(Color::Cyan),
            filler(),
            text("Host: ") | color(Color::GrayLight),
            text(cfg.host.empty() ? "None" : cfg.host) | bold | color(Color::Cyan),
            text("  |  Drive: ") | color(Color::GrayLight),
            text(mount_str) | color(mount_col),
            text(" ")
        }) | bgcolor(Color::RGB(26, 32, 44)) | border;

        // Tab views
        Element content;
        if (selected_tab == 0) {
            // Workspace & Actions
            content = vbox({
                vbox({
                    text("All-in-One Dev Workspace") | bold | color(Color::Cyan),
                    text("Mounts cluster storage, opens Dolphin file manager, and connects to an interactive compute shell.") | color(Color::GrayLight),
                    text("Auto-cleanup: Closing terminal or exiting unmounts drive and releases compute node automatically.") | color(Color::Yellow),
                    separator(),
                    hbox({btn_dev_cpu->Render(), btn_dev_gpu->Render()})
                }) | border,

                vbox({
                    text("Interactive Compute Shell") | bold | color(Color::Cyan),
                    text("Borrow an exclusive compute or GPU node without mounting. Auto-freed on close.") | color(Color::GrayLight),
                    separator(),
                    hbox({btn_shell_cpu->Render(), btn_shell_gpu->Render()})
                }) | border,

                vbox({
                    text("Remote JupyterLab") | bold | color(Color::Cyan),
                    text("Automatic SSH port tunneling and local browser launch.") | color(Color::GrayLight),
                    separator(),
                    hbox({btn_jup_head->Render(), btn_jup_slurm->Render(), btn_jup_gpu->Render()})
                }) | border,

                vbox({
                    text("Filesystem & Code Sync") | bold | color(Color::Cyan),
                    text("Mount cluster storage for local copy-pasting (Ctrl+C/V), or sync directories with rsync.") | color(Color::GrayLight),
                    separator(),
                    hbox({btn_mount->Render(), btn_unmount->Render(), btn_sync_push->Render(), btn_sync_pull->Render()})
                }) | border,

                hbox({
                    btn_quit->Render(),
                    filler(),
                    text("Press [q] or [Esc] to exit dashboard") | color(Color::GrayLight)
                })
            });
        } else if (selected_tab == 1) {
            // Cluster Monitor
            auto t_jobs = Table(jobs_data);
            t_jobs.SelectAll().Border(LIGHT);
            t_jobs.SelectRow(0).Decorate(bold);
            t_jobs.SelectRow(0).Decorate(color(Color::Cyan));

            auto t_nodes = Table(nodes_data);
            t_nodes.SelectAll().Border(LIGHT);
            t_nodes.SelectRow(0).Decorate(bold);
            t_nodes.SelectRow(0).Decorate(color(Color::Cyan));

            content = vbox({
                hbox({
                    text("My Active & Queued Jobs") | bold | color(Color::Cyan),
                    filler(),
                    btn_refresh->Render()
                }),
                t_jobs.Render(),
                separator(),
                text("Partition & Node Availability") | bold | color(Color::Cyan),
                t_nodes.Render()
            }) | border;
        } else {
            // Settings
            content = vbox({
                text("Cluster & Slurm Settings") | bold | color(Color::Cyan),
                text("Saved to ~/.config/cluster/config") | color(Color::GrayLight),
                separator(),
                hbox({text("Cluster Host:     ") | color(Color::GrayLight), comp_host->Render() | flex}),
                hbox({text("Slurm Partition:  ") | color(Color::GrayLight), comp_part->Render() | flex}),
                hbox({text("Slurm Account:    ") | color(Color::GrayLight), comp_account->Render() | flex}),
                hbox({text("Slurm QOS:        ") | color(Color::GrayLight), comp_qos->Render() | flex}),
                hbox({text("CPU Cores:        ") | color(Color::GrayLight), comp_cpus->Render() | flex}),
                hbox({text("Memory:           ") | color(Color::GrayLight), comp_mem->Render() | flex}),
                hbox({text("GPU Partition:    ") | color(Color::GrayLight), comp_gpu_part->Render() | flex}),
                hbox({text("GPU QOS:          ") | color(Color::GrayLight), comp_gpu_qos->Render() | flex}),
                hbox({text("Mount Point:      ") | color(Color::GrayLight), comp_mount->Render() | flex}),
                separator(),
                hbox({btn_save_cfg->Render(), text(" " + feedback_msg) | color(Color::Green)})
            }) | border;
        }

        // Bottom Footer
        auto footer = hbox({
            text(" [q/Esc] Quit "),
            text(" [r] Refresh "),
            text(" [d] Dev Workspace "),
            text(" [j] JupyterLab "),
            text(" [m] Toggle Mount ")
        }) | bgcolor(Color::RGB(26, 32, 44)) | color(Color::GrayLight);

        return vbox({
            header,
            tab_toggle->Render() | hcenter,
            separator(),
            content | flex,
            footer
        });
    });

    // Initial async monitor fetch
    trigger_refresh();

    screen.Loop(renderer);
    return 0;
}
